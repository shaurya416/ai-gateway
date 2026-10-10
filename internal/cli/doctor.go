package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/spf13/cobra"
)

// DoctorCmd runs offline environment and connectivity checks.
var DoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check environment, configuration, and gateway connectivity",
	Args:  cobra.NoArgs,
	RunE:  runDoctor,
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	if err := requireDefaultFormat(cmd); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "  Provider API Keys")

	// Detected with the gate serve registers providers through
	// (credentialedProviders), across every built-in provider. Checking five
	// variables reported "no provider API keys detected" to a deployment whose
	// credentials were for any of the others, which serve registers and routes
	// to. The five are always listed, as the startup banner lists them; any
	// other provider is listed when it is found.
	topProviders := []string{"openai", "anthropic", "gemini", "groq", "mistral"}
	detected := credentialedProviders()
	for _, name := range topProviders {
		if slices.Contains(detected, name) {
			_, _ = fmt.Fprintf(out, "    %s %s\n", Clr(ColorGreen, SymOK), name)
		} else {
			_, _ = fmt.Fprintf(out, "    %s %s\n", Clr(ColorDim, SymDASH), name)
		}
	}
	for _, name := range detected {
		if !slices.Contains(topProviders, name) {
			_, _ = fmt.Fprintf(out, "    %s %s\n", Clr(ColorGreen, SymOK), name)
		}
	}
	found := len(detected)

	if found == 0 {
		_, _ = fmt.Fprintf(out, "\n    %s no provider API keys detected\n", Clr(ColorYellow, SymWARN))
	} else {
		_, _ = fmt.Fprintf(out, "\n    %d found\n", found)
	}

	// Configuration check.
	//
	// configErr is the one finding doctor exits non-zero on. Everything else it
	// reports is a diagnostic an operator reads and judges — no provider keys is
	// a laptop, an unreachable gateway is one that is not running yet. An invalid
	// config is different in kind: `ferrogw validate` exits 1 on it and startup
	// refuses it, so a doctor that printed it in red and exited 0 made itself
	// useless as a pipeline gate while looking like one.
	var configErr error
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  Configuration")
	// GATEWAY_CONFIG and MASTER_KEY are trimmed because serve trims them
	// (bootstrap.configFilePath and bootstrap.ResolveMasterKey). Read verbatim,
	// a path an env file left a trailing space on failed here for a file serve
	// loads, and a whitespace-only key was reported set while serve ran with
	// none.
	cfgPath := strings.TrimSpace(os.Getenv("GATEWAY_CONFIG"))
	if cfgPath == "" {
		_, _ = fmt.Fprintf(out, "    %s GATEWAY_CONFIG not set (using defaults)\n", Clr(ColorDim, SymDASH))
	} else {
		// doctor answers "will this deployment work", so it applies the same
		// checks validate does — including validateReferences, which resolves
		// provider ids and plugin names against this binary. Reporting a config
		// green that `ferrogw validate` rejects would make doctor the least
		// trustworthy of the three commands that read the same file.
		cfg, err := config.LoadConfig(cfgPath)
		if err == nil {
			if err = config.ValidateConfig(*cfg); err == nil {
				err = validateReferences(*cfg)
			}
		}
		if err != nil {
			_, _ = fmt.Fprintf(out, "    %s %s: %v\n", Clr(ColorRed, SymFAIL), cfgPath, err)
			configErr = err
		} else {
			_, _ = fmt.Fprintf(out, "    %s %s (strategy=%s, targets=%d)\n",
				Clr(ColorGreen, SymOK), cfgPath, cfg.Strategy.Mode, len(cfg.Targets))
		}
	}

	// Master key check.
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  Auth")
	if strings.TrimSpace(os.Getenv("MASTER_KEY")) != "" {
		_, _ = fmt.Fprintf(out, "    %s MASTER_KEY is set\n", Clr(ColorGreen, SymOK))
	} else {
		_, _ = fmt.Fprintf(out, "    %s MASTER_KEY not set -- run 'ferrogw init' to generate one\n", Clr(ColorYellow, SymWARN))
	}

	// Connectivity check.
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  Gateway Connectivity")

	c := adminClientFromCmd(cmd)
	var h struct {
		Status string `json:"status"`
	}
	start := time.Now()
	// GetHealth, not Get: /health answers 503 while degraded, and a degraded
	// gateway is exactly what doctor exists to diagnose.
	err := c.GetHealth(cmd.Context(), "/health", &h)
	latency := time.Since(start)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(out, "    %s %s: %v\n", Clr(ColorRed, SymFAIL), c.BaseURL, err)
	case h.Status != "ok":
		_, _ = fmt.Fprintf(out, "    %s %s -- %s (%dms)\n", Clr(ColorYellow, SymWARN), c.BaseURL, h.Status, latency.Milliseconds())
	default:
		_, _ = fmt.Fprintf(out, "    %s %s -- healthy (%dms)\n", Clr(ColorGreen, SymOK), c.BaseURL, latency.Milliseconds())
	}

	_, _ = fmt.Fprintln(out)
	if configErr != nil {
		return fmt.Errorf("configuration is invalid: %w", configErr)
	}
	return nil
}
