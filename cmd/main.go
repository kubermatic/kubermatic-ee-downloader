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
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8c.io/kubermatic-ee-downloader/internal/dockerconfig"
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
			fmt.Fprintln(w, "TOOL\tVERSIONS\tOS\tARCH\tDESCRIPTION")
			for _, name := range tools.Names() {
				t := tools.KnownTools[name]
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					name,
					strings.Join(t.Tag, ", "),
					strings.Join(t.OS, ", "),
					strings.Join(t.Architectures, ", "),
					t.Description,
				)
			}
			return w.Flush()
		},
	}

	// --- get command ---
	var (
		version    string
		arch       string
		goos       string
		registry   string
		outputPath string
	)

	getCmd := &cobra.Command{
		Use:   "get <tool>",
		Short: "Download a tool binary from the OCI registry",
		Long: `Download a Kubermatic enterprise tool binary from its OCI registry.

Credentials are resolved in order: CLI flags, ~/.docker/config.json, interactive prompt.

If --version is not specified, the tool's first listed version is used. Available
tools and their versions can be listed with: kubermatic-ee-downloader list

Examples:
  kubermatic-ee-downloader get conformance-tester
  kubermatic-ee-downloader get conformance-tester --version v1.2.0 --output /usr/local/bin
  kubermatic-ee-downloader get conformance-tester --os linux --arch amd64
  kubermatic-ee-downloader get conformance-tester --username user --password pass`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: tools.Names(),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := newLogger(verbose)

			toolName := args[0]
			tool, ok := tools.KnownTools[toolName]
			if !ok {
				return fmt.Errorf("unknown tool %q — run 'kubermatic-ee-downloader list' to see available tools", toolName)
			}

			if registry == "" {
				registry = tool.Registry
			}

			if version == "" {
				if len(tool.Tag) > 0 {
					version = tool.Tag[0]
				} else {
					version = "latest"
				}
			}

			if arch != "" && !contains(tool.Architectures, arch) {
				return fmt.Errorf("unsupported arch %q for %q — supported: %s", arch, toolName, strings.Join(tool.Architectures, ", "))
			}
			if goos != "" && !contains(tool.OS, goos) {
				return fmt.Errorf("unsupported os %q for %q — supported: %s", goos, toolName, strings.Join(tool.OS, ", "))
			}

			if err := handleAuth(log, registry, &username, &password); err != nil {
				return err
			}

			log.WithFields(logrus.Fields{
				"tool":     toolName,
				"version":  version,
				"os":       goos,
				"arch":     arch,
				"registry": registry,
				"output":   outputPath,
			}).Info("Downloading tool")

			var outputName, tag string
			if tool.SimpleTag {
				tag = version
				outputName = tool.BinaryName
			} else {
				tag = fmt.Sprintf("%s-%s_%s", version, goos, arch)
				outputName = fmt.Sprintf("%s_%s-%s_%s", tool.BinaryName, version, goos, arch)
			}

			data, err := downloader.PullFromRegistry(cmd.Context(), log, registry, tag, tool.BinaryName, username, password)
			if err != nil {
				if !isUnauthorized(err) {
					return fmt.Errorf("pull failed: %w", err)
				}
				log.Warn("Authentication failed, please enter credentials")
				username, password = "", ""
				if err := promptCredentials(&username, &password); err != nil {
					return err
				}
				data, err = downloader.PullFromRegistry(cmd.Context(), log, registry, tag, tool.BinaryName, username, password)
				if err != nil {
					return fmt.Errorf("pull failed: %w", err)
				}
			}

			if err := downloader.Save(data, outputPath, outputName); err != nil {
				return fmt.Errorf("save failed: %w", err)
			}

			log.WithField("path", outputPath+"/"+outputName).Info("Download complete")
			return nil
		},
	}

	getCmd.Flags().StringVarP(&version, "version", "V", "", "Tool version (default: tool-specific or \"latest\")")
	getCmd.Flags().StringVar(&arch, "arch", currentArch(), "Target architecture (e.g. amd64, arm64)")
	getCmd.Flags().StringVar(&goos, "os", currentOS(), "Target operating system (e.g. linux, darwin, windows)")
	getCmd.Flags().StringVarP(&registry, "registry", "r", "", "Override OCI registry (default: tool's registry)")
	getCmd.Flags().StringVarP(&outputPath, "output", "o", ".", "Output directory")

	rootCmd.AddCommand(listCmd, getCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

func handleAuth(log *logrus.Logger, registry string, username, password *string) error {
	if *username != "" && *password != "" {
		return nil
	}

	// Try Docker config.json before prompting.
	creds, err := dockerconfig.GetCredentials(registry)
	if err != nil {
		log.WithError(err).Warn("Failed to read Docker config credentials")
	} else if creds != nil {
		log.Info("Using credentials from Docker config")
		if *username == "" {
			*username = creds.Username
		}
		if *password == "" {
			*password = creds.Password
		}
		if *username != "" && *password != "" {
			return nil
		}
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
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
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

func isUnauthorized(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized")
}

func promptCredentials(username, password *string) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Username: ")
	input, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read username: %w", err)
	}
	*username = strings.TrimSpace(input)

	fmt.Print("Password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}
	fmt.Println()
	*password = strings.TrimSpace(string(b))

	if *username == "" || *password == "" {
		return fmt.Errorf("username and password are required")
	}
	return nil
}

func currentOS() string {
	return runtime.GOOS
}

func currentArch() string {
	return runtime.GOARCH
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
