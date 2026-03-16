/*
Copyright 2026 The Kubermatic Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8c.io/kubermatic-ee-downloader/internal/downloader"
	"k8c.io/kubermatic-ee-downloader/internal/tools"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	var (
		username string
		password string
		verbose  bool
	)

	rootCmd := &cobra.Command{
		Use:          "kubermatic-downloader",
		Short:        "Download Kubermatic enterprise CLI tools",
		Version:      version,
		SilenceUsage: true,
	}

	rootCmd.PersistentFlags().StringVarP(&username, "username", "u", "", "Registry username")
	rootCmd.PersistentFlags().StringVarP(&password, "password", "p", "", "Registry password")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")

	// --- list command ---
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List available tools",
		RunE: func(_ *cobra.Command, _ []string) error {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "TOOL\tIMAGE\tDESCRIPTION")
			for _, name := range tools.Names() {
				t := tools.KnownTools[name]
				fmt.Fprintf(w, "%s\t%s\t%s\n", name, t.Registry, t.Description)
			}
			return w.Flush()
		},
	}

	// --- get command ---
	var (
		tag        string
		registry   string
		outputPath string
	)

	getCmd := &cobra.Command{
		Use:   "get <tool>",
		Short: "Download a tool binary from the OCI registry",
		Long: `Download a Kubermatic enterprise tool binary from its OCI registry.

Available tools can be listed with: kubermatic-downloader list

Examples:
  kubermatic-downloader get conformance-tester
  kubermatic-downloader get conformance-tester --tag v1.2.0 --output /usr/local/bin
  kubermatic-downloader get conformance-tester --username user --password pass`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: tools.Names(),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := newLogger(verbose)

			toolName := args[0]
			tool, ok := tools.KnownTools[toolName]
			if !ok {
				return fmt.Errorf("unknown tool %q — run 'kubermatic-downloader list' to see available tools", toolName)
			}

			if registry == "" {
				registry = tool.Registry
			}

			if err := handleAuth(log, &username, &password); err != nil {
				return err
			}

			log.WithFields(logrus.Fields{
				"tool":     toolName,
				"tag":      tag,
				"registry": registry,
				"output":   outputPath,
			}).Info("Downloading tool")

			data, err := downloader.PullFromRegistry(cmd.Context(), log, registry, tag, tool.BinaryName, username, password)
			if err != nil {
				return fmt.Errorf("pull failed: %w", err)
			}

			if err := downloader.Save(data, outputPath, tool.BinaryName); err != nil {
				return fmt.Errorf("save failed: %w", err)
			}

			log.WithField("path", outputPath+"/"+tool.BinaryName).Info("Download complete")
			return nil
		},
	}

	getCmd.Flags().StringVarP(&tag, "tag", "t", "latest", "Artifact tag")
	getCmd.Flags().StringVarP(&registry, "registry", "r", "", "Override OCI registry (default: tool's registry)")
	getCmd.Flags().StringVarP(&outputPath, "output", "o", ".", "Output directory")

	rootCmd.AddCommand(listCmd, getCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func handleAuth(log *logrus.Logger, username, password *string) error {
	if *username != "" && *password != "" {
		return nil
	}
	log.Info("Registry credentials required")
	reader := bufio.NewReader(os.Stdin)
	if *username == "" {
		fmt.Print("Username: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read username: %w", err)
		}
		*username = strings.TrimSpace(input)
	}
	if *password == "" {
		fmt.Print("Password: ")
		b, err := term.ReadPassword(syscall.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read password: %w", err)
		}
		fmt.Println()
		*password = strings.TrimSpace(string(b))
	}
	if *username == "" || *password == "" {
		return fmt.Errorf("username and password are required")
	}
	return nil
}

func newLogger(verbose bool) *logrus.Logger {
	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	if verbose {
		log.SetLevel(logrus.DebugLevel)
	} else {
		log.SetLevel(logrus.InfoLevel)
	}
	return log
}
