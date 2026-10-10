package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// adminLeaves returns every runnable command under the admin group.
func adminLeaves(cmd *cobra.Command) []*cobra.Command {
	var leaves []*cobra.Command
	for _, child := range cmd.Commands() {
		if child.HasSubCommands() {
			leaves = append(leaves, adminLeaves(child)...)
			continue
		}
		if child.Runnable() {
			leaves = append(leaves, child)
		}
	}
	return leaves
}

// A nil Args is cobra's ArbitraryArgs: the command accepts positional tokens it
// never reads. Under admin that is not a harmless no-op, because several of
// these commands change the running gateway. `admin keys create ci-bot` minted
// a key with no name — the label went to an argument nothing read — and printed
// it as created; `admin logs list 500` listed 50 rows. Every leaf must state
// what it accepts, so a stray token is refused before any request is sent.
func TestAdminCommandsRefuseArgumentsTheyDoNotRead(t *testing.T) {
	leaves := adminLeaves(AdminCmd)
	if len(leaves) == 0 {
		t.Fatal("found no admin commands to check")
	}
	for _, cmd := range leaves {
		t.Run(strings.ReplaceAll(cmd.CommandPath(), " ", "_"), func(t *testing.T) {
			if cmd.Args == nil {
				t.Fatalf("%q declares no Args policy, so it accepts positional arguments it never reads", cmd.CommandPath())
			}
			// One token more than the usage line names must be refused.
			want := len(strings.Fields(cmd.Use)) - 1
			args := make([]string, want+1)
			for i := range args {
				args[i] = "stray"
			}
			if err := cmd.ValidateArgs(args); err == nil {
				t.Fatalf("%q accepted %d positional arguments; its usage %q names %d", cmd.CommandPath(), len(args), cmd.Use, want)
			}
		})
	}
}
