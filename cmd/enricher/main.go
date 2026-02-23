package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/artemnikitin/firework/internal/enricher"
)

// APIGatewayV2Event is the subset of the API Gateway HTTP API v2 request
// envelope that we need to unwrap the body.
type APIGatewayV2Event struct {
	Version         string            `json:"version"`
	Headers         map[string]string `json:"headers"`
	Body            string            `json:"body"`
	IsBase64Encoded bool              `json:"isBase64Encoded"`
}

// WebhookEvent is the relevant subset of a GitHub push webhook payload.
type WebhookEvent struct {
	Repository struct {
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
	Ref string `json:"ref"` // e.g. "refs/heads/main"
}

// EventBridgeEvent is the minimal shape of an EventBridge scheduled event.
type EventBridgeEvent struct {
	Source     string `json:"source"`
	DetailType string `json:"detail-type"`
}

// isScheduledEvent returns true when the raw Lambda event is an EventBridge
// scheduled invocation (source == "aws.events").
func isScheduledEvent(raw json.RawMessage) bool {
	var ev EventBridgeEvent
	return json.Unmarshal(raw, &ev) == nil && ev.Source == "aws.events"
}

func handler(ctx context.Context, event json.RawMessage) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	targetBranch := os.Getenv("TARGET_BRANCH")
	if targetBranch == "" {
		targetBranch = "main"
	}

	var repoURL string

	if isScheduledEvent(event) {
		// Periodic re-sync triggered by EventBridge. Use CONFIG_REPO_URL env var
		// instead of a webhook payload since there is no push event.
		repoURL = os.Getenv("CONFIG_REPO_URL")
		if repoURL == "" {
			return fmt.Errorf("CONFIG_REPO_URL environment variable is required for scheduled invocations")
		}
		logger.Info("scheduled re-sync", "repo", repoURL, "branch", targetBranch)
	} else {
		// API Gateway HTTP API v2 wraps the webhook payload in an envelope.
		// Unwrap it if present; otherwise treat the event as a direct webhook
		// (useful for local testing / direct Lambda invocations).
		payload := event
		var envelope APIGatewayV2Event
		if err := json.Unmarshal(event, &envelope); err == nil && envelope.Body != "" {
			body := envelope.Body
			if envelope.IsBase64Encoded {
				decoded, err := base64.StdEncoding.DecodeString(body)
				if err != nil {
					return fmt.Errorf("decoding base64 body: %w", err)
				}
				body = string(decoded)
			}
			if secret := os.Getenv("GITHUB_WEBHOOK_SECRET"); secret != "" {
				if err := verifyGitHubWebhookSignature(secret, []byte(body), envelope.Headers); err != nil {
					return fmt.Errorf("verifying webhook signature: %w", err)
				}
			}
			payload = json.RawMessage(body)
			logger.Info("unwrapped API Gateway v2 envelope")
		}

		var wh WebhookEvent
		if err := json.Unmarshal(payload, &wh); err != nil {
			return fmt.Errorf("parsing webhook event: %w", err)
		}

		branch := branchFromRef(wh.Ref)
		if branch != targetBranch {
			logger.Info("ignoring push to non-target branch", "branch", branch, "target", targetBranch)
			return nil
		}

		repoURL = wh.Repository.CloneURL
		logger.Info("processing push event", "repo", repoURL, "branch", branch)
	}

	reader, err := enricher.NewGitReader(ctx, repoURL, targetBranch)
	if err != nil {
		return fmt.Errorf("cloning repo: %w", err)
	}
	defer reader.Close()

	// CONFIG_DIR allows the enricher config to live in a subdirectory of the repo
	// (e.g. "infra/firework"). Defaults to repo root.
	inputDir := reader.Dir()
	if sub := os.Getenv("CONFIG_DIR"); sub != "" {
		inputDir = filepath.Join(inputDir, sub)
	}

	cfg := enricher.Config{
		S3Bucket:           os.Getenv("S3_BUCKET"),
		S3Prefix:           os.Getenv("S3_PREFIX"),
		S3Region:           os.Getenv("S3_REGION"),
		S3EndpointURL:      os.Getenv("S3_ENDPOINT_URL"),
		SchedulerLambdaARN: os.Getenv("SCHEDULER_LAMBDA_ARN"),
		SchedulerRegion:    os.Getenv("SCHEDULER_REGION"),
		EC2Region:          os.Getenv("EC2_REGION"),
	}

	if cfg.S3Bucket == "" {
		return fmt.Errorf("S3_BUCKET environment variable is required")
	}

	result, err := enricher.Run(ctx, inputDir, cfg, logger)
	if err != nil {
		return fmt.Errorf("enrichment failed: %w", err)
	}

	logger.Info("enrichment complete", "nodes_written", len(result.NodeConfigs))
	return nil
}

// branchFromRef extracts the branch name from a Git ref.
// e.g. "refs/heads/main" -> "main"
func branchFromRef(ref string) string {
	const prefix = "refs/heads/"
	if strings.HasPrefix(ref, prefix) {
		return ref[len(prefix):]
	}
	return ref
}

func verifyGitHubWebhookSignature(secret string, body []byte, headers map[string]string) error {
	signature := headerValue(headers, "X-Hub-Signature-256")
	if signature == "" {
		return fmt.Errorf("missing X-Hub-Signature-256 header")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(strings.TrimSpace(signature)), []byte(expected)) {
		return fmt.Errorf("invalid X-Hub-Signature-256 header")
	}
	return nil
}

func headerValue(headers map[string]string, want string) string {
	for key, value := range headers {
		if strings.EqualFold(key, want) {
			return value
		}
	}
	return ""
}

func main() {
	lambda.Start(handler)
}
