package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// BuildTime is set at build time via -ldflags to YYYYMMDD.HHMM format.
var BuildTime = ""

// BuildVersion returns the effective version string for display.
func BuildVersion() string {
	if BuildTime != "" {
		return BuildTime
	}
	return Version
}

var rootCmd = &cobra.Command{
	Use:     "weclaw",
	Short:   "WeChat AI agent bridge",
	Long:    "weclaw bridges WeChat messages to AI agents via the iLink API.",
	Version: Version,
	RunE:    runStart, // default command is start
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
