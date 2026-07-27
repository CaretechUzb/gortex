package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// newDetachTestFlags mirrors the shape of the real `daemon start` flag set —
// a bool, strings, a uint and a repeatable slice — without touching the
// package-level command, so the table below can set flags freely.
func newDetachTestFlags() *pflag.FlagSet {
	fs := pflag.NewFlagSet("daemon start", pflag.ContinueOnError)
	fs.Bool("detach", false, "")
	fs.Bool("embeddings", false, "")
	fs.String("http-addr", "", "")
	fs.String("http-auth-token", "", "")
	fs.String("cors-origin", "*", "")
	fs.String("tools", "", "")
	fs.Uint64("backend-buffer-pool-mb", 0, "")
	fs.StringSlice("conversation-host", nil, "")
	return fs
}

func TestDetachedDaemonArgsForwardsChangedFlags(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{
			name: "nothing set forwards nothing",
			argv: nil,
			want: nil,
		},
		{
			// The reported bug: --http-addr vanished, so the port never listened.
			name: "http addr survives",
			argv: []string{"--detach", "--http-addr", "127.0.0.1:7411"},
			want: []string{"--http-addr=127.0.0.1:7411"},
		},
		{
			name: "detach itself is never forwarded",
			argv: []string{"--detach"},
			want: nil,
		},
		{
			name: "bool flag keeps its explicit value",
			argv: []string{"--embeddings=false"},
			want: []string{"--embeddings=false"},
		},
		{
			name: "non-default and defaulted-back values both forward",
			argv: []string{"--cors-origin=*", "--backend-buffer-pool-mb=512"},
			want: []string{"--backend-buffer-pool-mb=512", "--cors-origin=*"},
		},
		{
			name: "slice flag repeats instead of stringifying as a list",
			argv: []string{"--conversation-host=a.internal", "--conversation-host=b.internal"},
			want: []string{"--conversation-host=a.internal", "--conversation-host=b.internal"},
		},
		{
			// No shell sits between parent and child, so a value with spaces
			// or a leading dash must survive intact in --name=value form.
			name: "awkward values need no quoting",
			argv: []string{"--http-auth-token=-tok en"},
			want: []string{"--http-auth-token=-tok en"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newDetachTestFlags()
			if err := fs.Parse(tt.argv); err != nil {
				t.Fatalf("parse %v: %v", tt.argv, err)
			}
			got := detachedDaemonArgs(fs, fs)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("detachedDaemonArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A flag the child's `daemon start` doesn't know must be dropped: `install`
// and `daemon restart` reach the same spawn carrying their own flags, and a
// child that exits on "unknown flag" is worse than the flag not travelling.
func TestDetachedDaemonArgsDropsFlagsTheChildCannotParse(t *testing.T) {
	caller := pflag.NewFlagSet("install", pflag.ContinueOnError)
	caller.Bool("start", false, "")
	caller.String("agents", "", "")
	caller.String("log-level", "info", "")
	if err := caller.Parse([]string{"--start", "--agents=claude", "--log-level=debug"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	accepted := newDetachTestFlags()
	accepted.String("log-level", "info", "")

	got := detachedDaemonArgs(caller, accepted)
	want := []string{"--log-level=debug"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detachedDaemonArgs() = %q, want %q", got, want)
	}
}

func TestDetachedDaemonArgsNilCaller(t *testing.T) {
	if got := detachedDaemonArgs(nil, newDetachTestFlags()); got != nil {
		t.Errorf("detachedDaemonArgs(nil, ...) = %q, want nil", got)
	}
}

// The accepted set is what the child will actually parse, so it has to cover
// both halves: `daemon start`'s own flags and the root command's persistent
// ones. cobra merges those lazily, and the install / restart paths reach the
// spawn without `daemon start` ever having executed.
func TestDaemonStartAcceptedFlagsCoversLocalAndInherited(t *testing.T) {
	accepted := daemonStartAcceptedFlags()

	for _, name := range []string{"http-addr", "tools", "backend", "detach"} {
		if accepted.Lookup(name) == nil {
			t.Errorf("accepted set is missing local flag --%s", name)
		}
	}
	for _, name := range []string{"log-level", "config", "no-progress"} {
		if accepted.Lookup(name) == nil {
			t.Errorf("accepted set is missing inherited flag --%s", name)
		}
	}
}

// End-to-end through cobra: parsing a real `daemon start` invocation must
// reproduce every flag on the child's command line.
func TestDetachedDaemonArgsFromCobraInvocation(t *testing.T) {
	root := &cobra.Command{Use: "gortex"}
	root.PersistentFlags().String("log-level", "info", "")

	var captured []string
	start := &cobra.Command{
		Use: "start",
		RunE: func(cmd *cobra.Command, _ []string) error {
			captured = detachedDaemonArgs(cmd.Flags(), cmd.Flags())
			return nil
		},
	}
	start.Flags().Bool("detach", false, "")
	start.Flags().String("http-addr", "", "")
	daemon := &cobra.Command{Use: "daemon"}
	daemon.AddCommand(start)
	root.AddCommand(daemon)

	root.SetArgs([]string{"daemon", "start", "--detach", "--http-addr=127.0.0.1:7411", "--log-level=debug"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	want := []string{"--http-addr=127.0.0.1:7411", "--log-level=debug"}
	if !reflect.DeepEqual(captured, want) {
		t.Errorf("detachedDaemonArgs() = %q, want %q", captured, want)
	}
}
