package server

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

const machineUsage = `usage:
  sessionhub machine add <name> [--ssh-host H] [--herdr-host H] [--json]
  sessionhub machine rm <name> --yes
  sessionhub machine ls`

// RunMachine is `sessionhub machine add|rm|ls`. It works on the local database
// (SESSIONHUB_DB) directly, so it runs on the server host, not through the API.
func RunMachine(ctx context.Context, args []string) error {
	return runMachine(ctx, args, os.Stdout)
}

// AddOutput is what `sessionhub machine add --json` prints. `sessionhub join` parses it.
type AddOutput struct {
	Name      string `json:"name"`
	Token     string `json:"token"`
	ServerURL string `json:"server_url"`
}

func runMachine(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(machineUsage)
	}
	sub, rest := args[0], args[1:]
	wantArgs := map[string]int{"add": 1, "rm": 1, "ls": 0}
	n, ok := wantArgs[sub]
	if !ok {
		return fmt.Errorf("machine: unknown subcommand %q\n%s", sub, machineUsage)
	}
	fs := flag.NewFlagSet("sessionhub machine "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sshHost := fs.String("ssh-host", "", "host for ssh in resume commands (default: name)")
	herdrHost := fs.String("herdr-host", "", "host for herdr remote attach (default: name)")
	asJSON := fs.Bool("json", false, "print {name, token, server_url} as JSON")
	yes := fs.Bool("yes", false, "confirm deleting a machine and its data")
	pos, err := parseInterspersed(fs, rest)
	if err != nil {
		return fmt.Errorf("machine %s: %v\n%s", sub, err, machineUsage)
	}
	if sub != "rm" && *yes {
		return fmt.Errorf("machine %s: --yes applies to rm only\n%s", sub, machineUsage)
	}
	if sub != "add" && (*sshHost != "" || *herdrHost != "" || *asJSON) {
		return fmt.Errorf("machine %s: --ssh-host, --herdr-host, and --json apply to add only\n%s", sub, machineUsage)
	}
	if len(pos) != n {
		return fmt.Errorf("machine %s: want %d argument(s), got %d\n%s", sub, n, len(pos), machineUsage)
	}

	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("machine: %w", err)
	}
	st, err := store.Open(cfg.DB, store.Options{StaleAfter: cfg.StaleAfter})
	if err != nil {
		return fmt.Errorf("machine: %w", err)
	}
	defer st.Close()

	switch sub {
	case "add":
		return machineAdd(ctx, st, cfg, pos[0], *sshHost, *herdrHost, *asJSON, stdout)
	case "rm":
		if !*yes {
			in, err := st.MachineImpact(ctx, pos[0])
			if err != nil {
				return fmt.Errorf("machine rm: %w", err)
			}
			return fmt.Errorf("machine rm: removing %s deletes %d sessions, %d events, and %d reports, and revokes its token; nothing was deleted. Run `sessionhub machine rm %s --yes` to confirm",
				pos[0], in.Sessions, in.Events, in.Reports, pos[0])
		}
		if err := st.RemoveMachine(ctx, pos[0]); err != nil {
			return fmt.Errorf("machine rm: %w", err)
		}
		fmt.Fprintf(stdout, "removed machine %s; its token no longer works\n", pos[0])
		return nil
	default:
		return machineList(ctx, st, stdout)
	}
}

// parseInterspersed parses flags that may appear before or after positional
// arguments, which the flag package alone does not allow.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func machineAdd(ctx context.Context, st *store.Store, cfg Config, name, sshHost, herdrHost string, asJSON bool, stdout io.Writer) error {
	token, created, err := st.AddMachine(ctx, name, sshHost, herdrHost)
	if err != nil {
		return fmt.Errorf("machine add: %w", err)
	}
	if asJSON {
		b, err := json.Marshal(AddOutput{Name: name, Token: token, ServerURL: cfg.PublicURL})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", b)
		return err
	}
	m, err := findMachine(ctx, st, name)
	if err != nil {
		return err
	}
	verb := "added"
	if !created {
		verb = "rotated the token of"
	}
	fmt.Fprintf(stdout, "%s machine %s (ssh_host %s, herdr_host %s)\n", verb, name, m.SSHHost, m.HerdrHost)
	fmt.Fprintf(stdout, "token: %s\n", token)
	fmt.Fprintf(stdout, "server_url: %s\n", cfg.PublicURL)
	fmt.Fprintln(stdout, "Save the token now: it is not shown again, and the database keeps only its hash.")
	return nil
}

func findMachine(ctx context.Context, st *store.Store, name string) (api.Machine, error) {
	list, err := st.ListMachines(ctx)
	if err != nil {
		return api.Machine{}, err
	}
	for _, m := range list {
		if m.Name == name {
			return m, nil
		}
	}
	return api.Machine{}, fmt.Errorf("machine %q: %w", name, store.ErrNotFound)
}

func machineList(ctx context.Context, st *store.Store, stdout io.Writer) error {
	list, err := st.ListMachines(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no machines; add one with: sessionhub machine add <name>")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSSH_HOST\tHERDR_HOST\tLAST_SEEN\tMOVE_KEY")
	for _, m := range list {
		seen := "never"
		if m.LastSeen != nil {
			seen = m.LastSeen.Format(time.RFC3339)
		}
		key := m.MoveKey
		if key == "" {
			key = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Name, m.SSHHost, m.HerdrHost, seen, key)
	}
	return tw.Flush()
}
