// Command line interface for M365 Copilot.
// Single binary with subcommands: serve (API server), setup-wizard (browser-based setup).
// Default mode (no subcommand) runs CLI query or interactive mode.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/servers"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/setup"
)

const (
	// defaultRefreshTokenFile is the default path for the refresh token.
	defaultRefreshTokenFile = "data/tokens/rt_90day.txt"
	// defaultCacheFile is the default path for the token cache.
	defaultCacheFile = "data/tokens/token_cache.json"
	// defaultPort is the default port for the API server.
	defaultPort = 8000
)

func main() {
	// Check for subcommand
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			runServer(os.Args[2:])
			return
		case "setup-wizard":
			runSetupWizard(os.Args[2:])
			return
		}
	}

	// Default: CLI mode
	runCLI()
}

// runServer starts the HTTP API server.
func runServer(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", defaultPort, "Port to listen on")
	showVersion := fs.Bool("version", false, "Show version")
	fs.Parse(args)

	if *showVersion {
		fmt.Printf("M365 Copilot API Server v%s\n", models.Version)
		os.Exit(0)
	}

	config := models.LoadConfig()

	if config.TenantID == "" || config.UserOID == "" {
		log.Fatalf("Error: M365_TENANT_ID and M365_USER_OID environment variables are required")
	}

	tokenManager := auth.NewTokenManager(
		config.TenantID,
		config.ClientID,
		config.Scope,
		defaultRefreshTokenFile,
		defaultCacheFile,
	)

	apiServer := servers.NewAPIServer(config, tokenManager)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	errChan := make(chan error, 1)
	go func() {
		if err := apiServer.Start(*port); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-sigChan:
		log.Println("Shutting down server...")
		if err := apiServer.Stop(); err != nil {
			log.Printf("Error stopping server: %v", err)
		}
		log.Println("Server stopped")
	case err := <-errChan:
		log.Fatalf("Server error: %v", err)
	}
}

// runSetupWizard runs the browser-based setup wizard.
func runSetupWizard(args []string) {
	fs := flag.NewFlagSet("setup-wizard", flag.ExitOnError)
	file := fs.String("file", "data/setup.json", "Path to setup JSON file containing oid, tenant, and refresh_token")
	fs.Parse(args)

	if err := setup.Run(*file); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// runCLI runs the default CLI mode (single query or interactive).
func runCLI() {
	// Parse command-line flags
	model := flag.String("model", "auto", "Model to use (auto, quick, reasoning, gpt5.2, gpt5.3, gpt5.4, gpt5.5, claude)")
	reasoning := flag.Bool("reasoning", false, "Use reasoning mode")
	interactive := flag.Bool("i", false, "Interactive mode")
	noStream := flag.Bool("no-stream", false, "Disable streaming")
	listModels := flag.Bool("list-models", false, "List available models")
	showVersion := flag.Bool("version", false, "Show version")

	flag.Parse()

	// Handle version flag
	if *showVersion {
		fmt.Printf("M365Bridge v%s\n", models.Version)
		os.Exit(0)
	}

	// Load configuration
	config := models.LoadConfig()

	// Validate required configuration
	if config.TenantID == "" || config.UserOID == "" {
		fmt.Fprintf(os.Stderr, "Error: M365_TENANT_ID and M365_USER_OID environment variables are required\n")
		fmt.Fprintf(os.Stderr, "\nGet them from: https://graph.microsoft.com/v1.0/me (id and tenantId)\n")
		fmt.Fprintf(os.Stderr, "\nOr run the setup wizard to configure automatically\n")
		os.Exit(1)
	}

	// Initialize token manager
	tokenManager := auth.NewTokenManager(
		config.TenantID,
		config.ClientID,
		config.Scope,
		defaultRefreshTokenFile,
		defaultCacheFile,
	)

	// Create CLI server
	cliServer := servers.NewCLIServer(config, tokenManager)
	defer cliServer.Close()

	// Prepare options
	options := &servers.CLIOptions{
		Model:         *model,
		Reasoning:     *reasoning,
		Interactive:   *interactive,
		NoStream:      *noStream,
		Prompt:        flag.Arg(0),
		ListModels:    *listModels,
	}

	// Run CLI
	if err := cliServer.Run(options); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
