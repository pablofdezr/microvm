package storage

import "testing"

func TestValidMountPathAccepts(t *testing.T) {
	ok := []string{
		"",              // empty means "use the default"
		"/mnt/storage",  // the default itself
		"/data",         // a plain top-level dir
		"/mnt/a/b/c",    // nested
		"/srv/store-01", // digits and a dash
		"/x_y.z",        // the other allowed punctuation
	}
	for _, p := range ok {
		if !ValidMountPath(p) {
			t.Errorf("ValidMountPath(%q) = false, want true", p)
		}
	}
}

func TestValidMountPathRejectsInjection(t *testing.T) {
	// The whole reason this function exists: the value lands on a space-separated
	// kernel command line, so anything that could become a second parameter must
	// be refused. Each of these, if it reached the cmdline, would change the boot
	// rather than name a directory.
	bad := map[string]string{
		"space injects a param":      "/mnt/x init=/bin/sh",
		"tab is whitespace too":      "/mnt/x\tinit=/bin/sh",
		"newline ends the line":      "/mnt/x\nmicrovm.storage=rw",
		"relative, no leading slash": "mnt/storage",
		"climbs with ..":             "/mnt/../etc",
		"unclean double slash":       "/mnt//storage",
		"trailing slash is unclean":  "/mnt/storage/",
		"equals sign":                "/mnt=x",
		"quote":                      "/mnt/\"x",
		"shell metachar":             "/mnt/x;reboot",
	}
	for name, p := range bad {
		if ValidMountPath(p) {
			t.Errorf("%s: ValidMountPath(%q) = true, want false", name, p)
		}
	}
}

func TestValidMountPathLengthBound(t *testing.T) {
	long := "/"
	for i := 0; i < 300; i++ {
		long += "a"
	}
	if ValidMountPath(long) {
		t.Error("an over-long path should be refused")
	}
}

func TestMountPointDefaults(t *testing.T) {
	if got := (Mount{}).MountPoint(); got != DefaultMountPath {
		t.Errorf("empty mount = %q, want %q", got, DefaultMountPath)
	}
	if got := (Mount{MountPath: "/data"}).MountPoint(); got != "/data" {
		t.Errorf("set mount = %q, want /data", got)
	}
}
