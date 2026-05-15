package tui

import (
	"errors"
	"testing"
)

func TestResolvePagerCommand_PrefersPagerEnv(t *testing.T) {
	args, err := resolvePagerCommand(
		func(k string) string {
			if k == "PAGER" {
				return "moar -k"
			}
			return ""
		},
		func(string) (string, error) { return "/usr/bin/less", nil },
		"/tmp/x.log",
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []string{"moar", "-k", "/tmp/x.log"}
	if !slicesEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestResolvePagerCommand_FallsBackToLess(t *testing.T) {
	args, err := resolvePagerCommand(
		func(string) string { return "" },
		func(name string) (string, error) {
			if name == "less" {
				return "/usr/bin/less", nil
			}
			return "", errors.New("not found")
		},
		"/tmp/x.log",
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(args) == 0 || args[0] != "less" {
		t.Fatalf("expected less invocation, got %v", args)
	}
	if args[len(args)-1] != "/tmp/x.log" {
		t.Errorf("last arg = %q, want /tmp/x.log", args[len(args)-1])
	}
	// `-R` and `+F` should be in the argument list — they're what makes
	// the follow-mode + ANSI-colour-aware UX work.
	seen := map[string]bool{}
	for _, a := range args {
		seen[a] = true
	}
	for _, must := range []string{"-R", "+F"} {
		if !seen[must] {
			t.Errorf("less invocation missing %q in %v", must, args)
		}
	}
}

func TestResolvePagerCommand_NoPagerReturnsError(t *testing.T) {
	_, err := resolvePagerCommand(
		func(string) string { return "" },
		func(string) (string, error) { return "", errors.New("not found") },
		"/tmp/x.log",
	)
	if !errors.Is(err, errNoPager) {
		t.Errorf("err = %v, want errNoPager", err)
	}
}

func TestResolvePagerCommand_BlankPagerEnvTreatedAsUnset(t *testing.T) {
	// PAGER="   " (whitespace only) should fall through to `less`. This is
	// the behaviour that lets users `unset PAGER` indirectly via empty env
	// without accidentally invoking a no-op command line.
	args, err := resolvePagerCommand(
		func(k string) string {
			if k == "PAGER" {
				return "   "
			}
			return ""
		},
		func(name string) (string, error) {
			if name == "less" {
				return "/usr/bin/less", nil
			}
			return "", errors.New("not found")
		},
		"/tmp/x.log",
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if args[0] != "less" {
		t.Errorf("args[0] = %q, want less", args[0])
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
