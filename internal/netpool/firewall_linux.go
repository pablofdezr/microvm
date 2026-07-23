//go:build linux

package netpool

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"text/template"
)

// The firewall is installed once, at daemon start, and never touched again as
// sandboxes come and go. Every rule matches on the TAP name prefix, so a new
// sandbox is covered by rules that already exist. The alternative -- adding
// rules per VM -- means a sandbox is briefly unfiltered between its interface
// appearing and its rule landing, and it leaks rules whenever teardown is
// interrupted. Neither is acceptable when the code inside is assumed hostile.
//
// Two things are deliberately absent:
//
//   - No `policy drop` anywhere. These chains share the forward and input hooks
//     with whatever else runs on the host -- Docker in particular installs its
//     own forwarding rules. A drop policy here would silently break every other
//     bridge on the box. Instead every rule is scoped to iifname/oifname
//     "fctap*", so nothing outside a sandbox can match.
//   - No per-sandbox state. The subnets are covered as a range.
const rulesetTemplate = `
# Managed by microvmd. Do not edit: this table is replaced wholesale on start.
table inet {{.Table}} {
	# Destinations a sandbox must never reach. Blocking the RFC1918 ranges also
	# blocks sandbox-to-sandbox traffic, since the pool itself lives inside one
	# of them -- that is intended, not collateral.
	set blocked4 {
		type ipv4_addr
		flags interval
		elements = {
			0.0.0.0/8,          # "this network"
			10.0.0.0/8,         # RFC1918 private
			100.64.0.0/10,      # CGNAT: the ISP's own infrastructure
			127.0.0.0/8,        # host loopback
			169.254.0.0/16,     # link-local, and cloud metadata at 169.254.169.254
			172.16.0.0/12,      # RFC1918 private, and where the sandbox pool lives
			192.0.0.0/24,       # IETF protocol assignments
			192.168.0.0/16,     # RFC1918 private: the LAN this host sits on
			198.18.0.0/15,      # benchmarking range
			224.0.0.0/4,        # multicast
			240.0.0.0/4         # reserved
		}
	}

	set blocked6 {
		type ipv6_addr
		flags interval
		elements = {
			::1/128,            # loopback
			fc00::/7,           # unique local
			fe80::/10,          # link-local
			ff00::/8            # multicast
		}
	}

	chain forward {
		type filter hook forward priority filter; policy accept;

		# Return traffic for connections a sandbox opened itself.
		iifname "{{.TapPrefix}}*" ct state established,related accept
		oifname "{{.TapPrefix}}*" ct state established,related accept

		# Nothing may initiate a connection *into* a sandbox. A sandbox is
		# reached over vsock by the daemon, never over the network.
		oifname "{{.TapPrefix}}*" drop

		# Egress: everything private is refused, the public internet is allowed.
		iifname "{{.TapPrefix}}*" ip daddr @blocked4 {{.LogPrefix}}drop
		iifname "{{.TapPrefix}}*" ip6 daddr @blocked6 {{.LogPrefix}}drop
		iifname "{{.TapPrefix}}*" accept
	}

	chain input {
		type filter hook input priority filter; policy accept;

		# The host itself is not a service a sandbox may talk to -- not SSH, not
		# the daemon, not the gateway address on its own TAP. The guest needs
		# the gateway only as a next hop, which is forwarding, not input.
		#
		# ARP is unaffected: it is resolved below the inet family's IP hooks, so
		# the guest can still find its gateway's hardware address.
		iifname "{{.TapPrefix}}*" {{.LogPrefix}}drop
	}
}

table ip {{.Table}}-nat {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;

		# Masquerade sandbox traffic leaving the box. Excluding TAP interfaces on
		# egress keeps this from touching traffic between sandboxes, which the
		# forward chain has already dropped anyway.
		ip saddr {{.PoolCIDR}} oifname != "{{.TapPrefix}}*" masquerade
	}
}
`

// Firewall installs and removes the host ruleset for the sandbox network.
type Firewall struct {
	table    string
	poolCIDR netip.Prefix
	logDrops bool
	log      *slog.Logger
}

// FirewallConfig configures the ruleset.
type FirewallConfig struct {
	// PoolCIDR is the base network the sandboxes are allocated from.
	PoolCIDR netip.Prefix
	// Table names the nftables tables. Defaults to "microvm".
	Table string
	// LogDrops logs every blocked packet. Useful when diagnosing why a sandbox
	// cannot reach something; too noisy to leave on under load, since hostile
	// code can generate drops as fast as it can send.
	LogDrops bool
}

// NewFirewall validates the configuration and returns an installer.
func NewFirewall(cfg FirewallConfig, log *slog.Logger) (*Firewall, error) {
	if !cfg.PoolCIDR.IsValid() || !cfg.PoolCIDR.Addr().Is4() {
		return nil, fmt.Errorf("pool CIDR %s must be a valid IPv4 prefix", cfg.PoolCIDR)
	}
	if cfg.Table == "" {
		cfg.Table = "microvm"
	}

	// The pool must sit inside a private range. If it did not, the blocked sets
	// would not cover sandbox-to-sandbox traffic and the guests could reach each
	// other directly.
	if !cfg.PoolCIDR.Addr().IsPrivate() {
		return nil, fmt.Errorf("pool CIDR %s must be within a private range", cfg.PoolCIDR)
	}

	return &Firewall{
		table:    cfg.Table,
		poolCIDR: cfg.PoolCIDR.Masked(),
		logDrops: cfg.LogDrops,
		log:      log,
	}, nil
}

// Install applies the ruleset, replacing any previous copy of it.
//
// The whole ruleset is applied in a single atomic nft transaction: there is no
// window in which the tables exist half-configured and a sandbox could route
// somewhere it should not.
func (f *Firewall) Install() error {
	if err := requireNft(); err != nil {
		return err
	}

	ruleset, err := f.render()
	if err != nil {
		return err
	}

	// Deleting before adding makes the operation idempotent across restarts.
	// `destroy` does not error on a missing table, unlike `delete`, so a first
	// run and a restart take the same path.
	script := fmt.Sprintf("destroy table inet %s\ndestroy table ip %s-nat\n%s",
		f.table, f.table, ruleset)

	if err := runNft(script); err != nil {
		return fmt.Errorf("install ruleset: %w", err)
	}

	if err := enableForwarding(); err != nil {
		return err
	}

	if err := f.allowThroughForeignChains(); err != nil {
		return err
	}

	f.log.Info("firewall installed", "table", f.table, "pool", f.poolCIDR, "log_drops", f.logDrops)
	return nil
}

// compatComment tags the rules we add to chains belonging to other software, so
// we can find and replace exactly our own on reinstall without disturbing
// theirs.
const compatComment = "microvm-compat"

// allowThroughForeignChains makes sandbox traffic survive filter chains this
// daemon does not own.
//
// Our own chains accept sandbox egress, but that is not enough. In netfilter an
// accept verdict means "stop evaluating *this* chain", not "deliver the packet":
// every other base chain on the same hook still runs, and a single drop anywhere
// is final. Docker sets the filter FORWARD policy to drop and only accepts
// traffic for its own bridges, so with Docker installed every sandbox packet is
// dropped after our chain has already accepted it -- silently, and with the
// symptom appearing as "no internet in the guest" rather than as anything
// pointing at Docker.
//
// DOCKER-USER is the chain Docker documents for exactly this and never flushes.
// An accept there applies only to the ip filter table, so our own drops in the
// inet table continue to protect the private ranges: this restores reachability
// without widening what a sandbox may reach.
func (f *Firewall) allowThroughForeignChains() error {
	const (
		family = "ip"
		table  = "filter"
		chain  = "DOCKER-USER"
	)

	if !chainExists(family, table, chain) {
		// No Docker. If something else is dropping on the forward hook, the
		// symptom is identical and just as confusing, so say so rather than let
		// it be discovered as a mysterious lack of connectivity.
		if dropper, found := foreignForwardDropper(f.table); found {
			f.log.Warn("a foreign chain drops on the forward hook; sandbox egress may not work",
				"chain", dropper,
				"hint", "add an accept for iifname \""+TapPrefix+"*\" to that chain")
		}
		return nil
	}

	if err := f.removeCompatRules(family, table, chain); err != nil {
		return err
	}

	// Insert rather than add: these go at the top of the chain, ahead of
	// anything Docker or a user has put there.
	rules := []string{
		fmt.Sprintf(`insert rule %s %s %s oifname "%s*" ct state established,related accept comment "%s"`,
			family, table, chain, TapPrefix, compatComment),
		fmt.Sprintf(`insert rule %s %s %s iifname "%s*" accept comment "%s"`,
			family, table, chain, TapPrefix, compatComment),
	}
	for _, rule := range rules {
		if err := runNft(rule); err != nil {
			return fmt.Errorf("allow sandbox traffic through %s: %w", chain, err)
		}
	}

	f.log.Info("added compatibility rules to a foreign chain",
		"chain", chain, "reason", "its table drops forwarded traffic by policy")
	return nil
}

// removeCompatRules deletes the rules we previously added to a foreign chain,
// leaving that chain's own rules untouched. Without this, every restart would
// stack another copy.
func (f *Firewall) removeCompatRules(family, table, chain string) error {
	handles, err := compatRuleHandles(family, table, chain)
	if err != nil {
		return err
	}
	for _, h := range handles {
		cmd := fmt.Sprintf("delete rule %s %s %s handle %d", family, table, chain, h)
		if err := runNft(cmd); err != nil {
			return fmt.Errorf("remove stale compat rule %d from %s: %w", h, chain, err)
		}
	}
	return nil
}

// compatRuleHandles returns the handles of rules in a foreign chain that carry
// our comment.
func compatRuleHandles(family, table, chain string) ([]int, error) {
	var out bytes.Buffer
	cmd := exec.Command("nft", "-a", "list", "chain", family, table, chain)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("list chain %s: %w", chain, err)
	}

	var handles []int
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, `comment "`+compatComment+`"`) {
			continue
		}
		_, rest, found := strings.Cut(line, "# handle ")
		if !found {
			continue
		}
		var h int
		if _, err := fmt.Sscanf(strings.TrimSpace(rest), "%d", &h); err != nil {
			continue
		}
		handles = append(handles, h)
	}
	return handles, nil
}

func chainExists(family, table, chain string) bool {
	return exec.Command("nft", "list", "chain", family, table, chain).Run() == nil
}

// foreignForwardDropper reports the first base chain on the forward hook, other
// than ours, whose policy is drop.
func foreignForwardDropper(ownTable string) (string, bool) {
	var out bytes.Buffer
	cmd := exec.Command("nft", "list", "ruleset")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false
	}

	var currentTable, currentChain string
	for _, line := range strings.Split(out.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "table "):
			currentTable = trimmed
		case strings.HasPrefix(trimmed, "chain "):
			currentChain = strings.TrimSuffix(strings.TrimPrefix(trimmed, "chain "), " {")
		case strings.Contains(trimmed, "hook forward") && strings.Contains(trimmed, "policy drop"):
			if strings.Contains(currentTable, ownTable) {
				continue
			}
			return fmt.Sprintf("%s / %s", strings.TrimSuffix(currentTable, " {"), currentChain), true
		}
	}
	return "", false
}

// Remove tears the ruleset down. Sandboxes must already be stopped: removing
// the rules while a guest is running would leave it with unfiltered egress.
func (f *Firewall) Remove() error {
	script := fmt.Sprintf("destroy table inet %s\ndestroy table ip %s-nat\n", f.table, f.table)
	if err := runNft(script); err != nil {
		return fmt.Errorf("remove ruleset: %w", err)
	}

	// Our rules in other software's chains outlive our own tables, so they have
	// to be cleaned up explicitly or they accumulate across restarts.
	if chainExists("ip", "filter", "DOCKER-USER") {
		if err := f.removeCompatRules("ip", "filter", "DOCKER-USER"); err != nil {
			return err
		}
	}

	f.log.Info("firewall removed", "table", f.table)
	return nil
}

func (f *Firewall) render() (string, error) {
	logPrefix := ""
	if f.logDrops {
		logPrefix = `log prefix "microvm-drop: " level info `
	}

	tmpl, err := template.New("ruleset").Parse(rulesetTemplate)
	if err != nil {
		return "", fmt.Errorf("parse ruleset template: %w", err)
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, struct {
		Table     string
		TapPrefix string
		PoolCIDR  string
		LogPrefix string
	}{
		Table:     f.table,
		TapPrefix: TapPrefix,
		PoolCIDR:  f.poolCIDR.String(),
		LogPrefix: logPrefix,
	})
	if err != nil {
		return "", fmt.Errorf("render ruleset: %w", err)
	}
	return buf.String(), nil
}

func requireNft() error {
	if _, err := exec.LookPath("nft"); err != nil {
		return fmt.Errorf("nft not found: install nftables (the sandbox network cannot be secured without it)")
	}
	return nil
}

// runNft feeds a script to nft on stdin, which applies it as one transaction.
func runNft(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// enableForwarding turns on IPv4 forwarding, without which a sandbox's packets
// reach the host and stop there.
func enableForwarding() error {
	const path = "/proc/sys/net/ipv4/ip_forward"

	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if strings.TrimSpace(string(current)) == "1" {
		return nil
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable ip forwarding: %w", err)
	}
	return nil
}

// DumpRuleset returns the live ruleset for the table, for diagnostics and for
// tests that assert the rules are actually present rather than merely applied
// without error.
func DumpRuleset(table string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("nft", "list", "table", "inet", table)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}
