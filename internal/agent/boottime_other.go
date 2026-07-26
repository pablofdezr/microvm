//go:build !linux

package agent

import "time"

// Off Linux there is no guest to have booted: the agent runs in -listen=tcp
// development mode inside an ordinary process, where "how long did the kernel
// take" is not a question about this program. Reporting nothing is the honest
// answer, and the host reads an absent split as unavailable rather than as zero.

func bootEnv() []string { return nil }

func readBootEnv() (kernel, init time.Duration) { return 0, 0 }
