// Package servers provides CLI and HTTP server implementations for M365 Copilot.
// This file contains the CLI server for interactive and single-query modes.
package servers

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/auth"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/client"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/models"
	"github.com/KilimcininKorOglu/M365Bridge/pkg/payload"
)

// CLIServer handles command-line interface operations.
type CLIServer struct {
	config       *models.Config
	tokenManager *auth.TokenManager
	m365Client   *client.M365Client
}

// NewCLIServer creates a new CLI server instance.
func NewCLIServer(config *models.Config, tokenManager *auth.TokenManager) *CLIServer {
	return &CLIServer{
		config:       config,
		tokenManager: tokenManager,
	}
}

// Run executes the CLI with the given options.
func (cli *CLIServer) Run(options *CLIOptions) error {
	// Initialize client if needed
	if cli.m365Client == nil {
		cli.m365Client = client.NewM365Client(cli.tokenManager)
	}

	// Handle different modes
	switch {
	case options.ListModels:
		return cli.listModels()
	case options.Interactive:
		return cli.runInteractive(options)
	default:
		return cli.runSingleQuery(options)
	}
}

// CLIOptions represents command-line options.
type CLIOptions struct {
	Model       string
	Reasoning   bool
	Interactive bool
	NoStream    bool
	Prompt      string
	ListModels  bool
}

// listModels prints all available models.
func (cli *CLIServer) listModels() error {
	fmt.Println("Available models:")
	for key, cfg := range models.ModelRegistry {
		desc := cfg.Tone
		if cfg.Override != "" {
			desc += fmt.Sprintf(" (%s)", cfg.Override)
		}
		fmt.Printf("  %-12s - %s\n", key, desc)
	}
	return nil
}

// runInteractive starts the interactive mode.
func (cli *CLIServer) runInteractive(options *CLIOptions) error {
	fmt.Printf("M365 Copilot v%s (Interactive Mode)\n", models.Version)
	fmt.Printf("Model: %s\n", options.Model)
	fmt.Println("Exit: Ctrl+C")

	reader := bufio.NewReader(os.Stdin)
	messageHistory := []payload.Message{}

	for {
		fmt.Print("\n> ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}

		text := strings.TrimSpace(input)
		if text == "" {
			continue
		}

		cfg := models.LookupModel(options.Model)
		tone := cfg.Tone
		if options.Reasoning {
			tone = "Reasoning"
		}

		var result string
		if options.NoStream {
			result, err = cli.m365Client.Chat(text, tone, cfg.Override, "", cli.config.UserOID, cli.config.TenantID)
			if err == nil && result != "" {
				fmt.Println(result)
			}
		} else {
			fmt.Println()
			result, err = cli.streamToStdout(text, tone, cfg.Override, "")
			fmt.Println()
		}

		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}

		messageHistory = append(messageHistory, payload.Message{Role: "user", Content: text})
		messageHistory = append(messageHistory, payload.Message{Role: "assistant", Content: result})
	}
}

// runSingleQuery executes a single query and prints the result.
func (cli *CLIServer) runSingleQuery(options *CLIOptions) error {
	if options.Prompt == "" {
		return fmt.Errorf("prompt is required for single query mode")
	}

	cfg := models.LookupModel(options.Model)
	tone := cfg.Tone
	if options.Reasoning {
		tone = "Reasoning"
	}

	var result string
	var err error

	if options.NoStream {
		result, err = cli.m365Client.Chat(options.Prompt, tone, cfg.Override, "", cli.config.UserOID, cli.config.TenantID)
		if err == nil && result != "" {
			fmt.Println(result)
		}
	} else {
		result, err = cli.streamToStdout(options.Prompt, tone, cfg.Override, "")
	}

	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}

	return nil
}

// streamToStdout streams the response directly to stdout and returns the full text.
func (cli *CLIServer) streamToStdout(text, tone, gptOverride, convID string) (string, error) {
	fullText := ""
	ch := cli.m365Client.ChatStreamGen(text, tone, gptOverride, convID, cli.config.UserOID, cli.config.TenantID)
	for chunk := range ch {
		if chunk.Error != nil {
			return fullText, chunk.Error
		}
		if !chunk.IsFinal {
			fmt.Print(chunk.Text)
			fullText += chunk.Text
		}
	}
	return fullText, nil
}

// Close cleans up resources.
func (cli *CLIServer) Close() error {
	if cli.m365Client != nil {
		return cli.m365Client.Close()
	}
	return nil
}
