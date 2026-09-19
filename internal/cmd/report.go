package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/diagnostics"
)

// PrivateReportRequested also covers malformed invocations: they must never
// send telemetry containing arguments before report consent is established.
func PrivateReportRequested(args []string) bool {
	for _, arg := range args {
		if arg == "report" || arg == "reports" {
			return true
		}
	}
	return false
}

func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "report", Aliases: []string{"reports"}, GroupID: "system",
		Short: "Preview and optionally send a private technical report",
		Long:  "Prepare technical events locally, review the exact JSON, then separately consent to upload. Only the agent UUID identifies the installation inside the report. Unrecognized log text is omitted. No account, IP, paths, credentials or raw logs are attached.",
		Args:  cobra.NoArgs,
		// The inherited pre-run initializes the authenticated client and Sentry.
		PersistentPreRun: func(_ *cobra.Command, _ []string) {},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReport(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), reportActions{
				collect: collectPrivateReport, send: diagnostics.Send, save: savePrivateReport,
			})
		},
	}
	return cmd
}

type reportActions struct {
	collect func() ([]byte, error)
	send    func(context.Context, []byte) error
	save    func([]byte) (string, error)
}

func collectPrivateReport() ([]byte, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, errQuietExit
	}
	report, err := diagnostics.Collect(&cfg, Version)
	if err != nil {
		return nil, errQuietExit
	}
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil || len(payload) > diagnostics.MaxReportBytes {
		return nil, errQuietExit
	}
	return payload, nil
}

func reportAnswer(r *bufio.Reader) string {
	// Bound answers and require a complete line. EOF never implies consent.
	line, err := r.ReadSlice('\n')
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(line)))
}

func runReport(ctx context.Context, in io.Reader, out io.Writer, actions reportActions) error {
	r := bufio.NewReaderSize(in, 4096)
	fmt.Fprintln(out, "Prepare a local technical report? Only the agent UUID, system metrics and recognized technical events are included. Unknown log text is omitted. Nothing is sent yet. [yes/N]")
	if reportAnswer(r) != "yes" {
		return nil
	}
	payload, err := actions.collect()
	if err != nil {
		fmt.Fprintln(out, "Cannot prepare report. Check local configuration and the agent UUID.")
		return errQuietExit
	}
	fmt.Fprintln(out, "Exact report content:")
	if _, err := fmt.Fprintf(out, "%s\n", payload); err != nil {
		return errQuietExit
	}
	fmt.Fprintln(out, "Choose: save / send / cancel [cancel]")
	switch reportAnswer(r) {
	case "save":
		return keepPrivateReport(out, actions.save, payload)
	case "send":
		fmt.Fprintln(out, "Send exactly this report to Unarr support? The agent UUID can be linked to your installation. The network connection reveals your source IP to the services carrying it, but the IP is not included in the report. [yes/N]")
		if reportAnswer(r) != "yes" {
			return nil
		}
		if err := actions.send(ctx, payload); err != nil {
			fmt.Fprintln(out, "Report not delivered. Private delivery may be unavailable; no fallback service will be used.")
			if saveErr := keepPrivateReport(out, actions.save, payload); saveErr != nil {
				return saveErr
			}
			return errQuietExit
		}
		fmt.Fprintln(out, "Report sent.")
	}
	return nil
}

func keepPrivateReport(out io.Writer, save func([]byte) (string, error), payload []byte) error {
	path, err := save(payload)
	if err != nil {
		fmt.Fprintln(out, "Cannot save report in the current directory.")
		return errQuietExit
	}
	fmt.Fprintf(out, "Report saved locally: %s\n", path)
	return nil
}

func savePrivateReport(payload []byte) (string, error) {
	// Exclusive creation with private permissions from birth on each platform.
	f, err := createPrivateReportFile()
	if err != nil {
		return "", err
	}
	name := f.Name()
	_, writeErr := f.Write(payload)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return "", errQuietExit
	}
	return name, nil
}
