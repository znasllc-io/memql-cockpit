// `memql worker consent ...` -- operator-facing controls
// for the consent gate (memql-cockpit#64). Talks to a running
// worker daemon over the Unix socket the worker opens at startup.
//
// Subcommands:
//
//	consent grant --window=<duration> [--strict]   Open a window for the requested duration
//	consent revoke                                  Close any active window IMMEDIATELY
//	consent status                                  Show current state
//	consent watch                                   Live tail of dispatch / grant / revoke events
//
// The duration is anything time.ParseDuration accepts: 5m, 1h, 30s.

package worker

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
)

func handleConsentCmd(args []string) {
	if len(args) == 0 {
		printConsentUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "grant":
		handleConsentGrant(args[1:])
	case "revoke":
		handleConsentRevoke(args[1:])
	case "status":
		handleConsentStatus(args[1:])
	case "watch":
		handleConsentWatch(args[1:])
	case "-h", "--help", "help":
		printConsentUsage()
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown consent subcommand %q\n\n", args[0])
		printConsentUsage()
		os.Exit(1)
	}
}

func printConsentUsage() {
	fmt.Println("Usage: memql worker consent <subcommand>")
	fmt.Println("")
	fmt.Println("Subcommands:")
	fmt.Println("  grant --window=<duration> [--strict]   Open a consent window (e.g. --window=1h)")
	fmt.Println("  revoke                                  Close any active window IMMEDIATELY")
	fmt.Println("  status                                  Show the current consent state")
	fmt.Println("  watch                                   Live tail of grant / revoke / dispatch events")
	fmt.Println("")
	fmt.Println("Environment:")
	fmt.Println("  MEMQL_WORKER_CONSENT_SOCKET   Override the socket path (default: ~/.memql/worker.sock)")
}

func consentClient() *consent.Client {
	return &consent.Client{
		Path:    consent.DefaultSocketPath(),
		Timeout: 2 * time.Second,
	}
}

func handleConsentGrant(args []string) {
	fs := flag.NewFlagSet("worker consent grant", flag.ExitOnError)
	window := fs.Duration("window", 0, "consent window duration (e.g. 5m, 1h, 30s); REQUIRED")
	strict := fs.Bool("strict", false, "require per-action approval for high-risk calls (v1: enforcement is a follow-up under #64)")
	_ = fs.Parse(args)

	if *window <= 0 {
		fmt.Fprintln(os.Stderr, "ERROR: --window is required and must be > 0 (e.g. --window=1h)")
		os.Exit(1)
	}
	if *window > 24*time.Hour {
		fmt.Fprintf(os.Stderr, "WARNING: long window (%s) -- consider a shorter duration for security.\n", *window)
	}

	// The CLI grant path never sets a strict-mode region -- the
	// in-region exemption (memql-cockpit#131) is configured through
	// the TUI Workers-pane region picker, which needs the visual
	// box-draw surface to be usable. CLI callers get plain strict
	// mode (every high-risk call gated).
	resp, err := consentClient().Grant(*window, *strict, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		fmt.Fprintln(os.Stderr, "Hint: is the worker running? Start it with `memql worker run`.")
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", resp.Error)
		os.Exit(1)
	}
	fmt.Printf("Consent granted. Window: %s. Expires at: %s.\n",
		*window, resp.Status.ExpiresAt.Local().Format(time.RFC3339))
	if *strict {
		fmt.Println("Strict mode: ON (per-action approval ships under #64 follow-up).")
	}
}

func handleConsentRevoke(_ []string) {
	resp, err := consentClient().Revoke()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", resp.Error)
		os.Exit(1)
	}
	fmt.Println("Consent revoked. Future worker tool calls will be denied until you grant a new window.")
}

func handleConsentStatus(_ []string) {
	resp, err := consentClient().Status()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		fmt.Fprintln(os.Stderr, "Hint: is the worker running? Start it with `memql worker run`.")
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", resp.Error)
		os.Exit(1)
	}
	if !resp.Status.Granted {
		fmt.Println("Consent: NOT GRANTED")
		fmt.Println("Run `memql worker consent grant --window=<duration>` to open a window.")
		return
	}
	remaining := time.Until(resp.Status.ExpiresAt).Round(time.Second)
	fmt.Println("Consent: GRANTED")
	fmt.Printf("  Expires at:   %s\n", resp.Status.ExpiresAt.Local().Format(time.RFC3339))
	fmt.Printf("  Remaining:    %s\n", remaining)
	if resp.Status.Window > 0 {
		fmt.Printf("  Window:       %s\n", resp.Status.Window)
	}
	if resp.Status.Strict {
		fmt.Println("  Strict mode:  ON")
	}
}

func handleConsentWatch(_ []string) {
	fmt.Fprintln(os.Stderr, "Watching consent events (Ctrl+C to exit)...")
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()
	err := consentClient().Watch(ctx, func(line []byte) {
		// First message is the initial status response; the rest
		// are events. We pretty-print both shapes here without
		// over-modeling -- the operator can use jq if they want
		// structure.
		var probe map[string]any
		if jerr := json.Unmarshal(line, &probe); jerr != nil {
			fmt.Println(string(line))
			return
		}
		if k, ok := probe["kind"].(string); ok && k != "" {
			fmt.Printf("[%s] %s", time.Now().Format("15:04:05"), k)
			if tool, _ := probe["tool"].(string); tool != "" {
				fmt.Printf(" %s.%v", tool, probe["action"])
			}
			if class, _ := probe["class"].(string); class != "" {
				fmt.Printf(" class=%s", class)
			}
			if allowed, ok := probe["allowed"].(bool); ok {
				outcome := "DENIED"
				if allowed {
					outcome = "ALLOWED"
				}
				fmt.Printf(" %s", outcome)
			}
			if reason, _ := probe["reason"].(string); reason != "" {
				fmt.Printf(" -- %s", reason)
			}
			fmt.Println()
			return
		}
		// Initial Response shape.
		out, _ := json.MarshalIndent(probe, "", "  ")
		fmt.Println(string(out))
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}
