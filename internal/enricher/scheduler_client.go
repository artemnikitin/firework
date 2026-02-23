package enricher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"

	"github.com/artemnikitin/firework/internal/config"
)

// schedulerRequest is sent to the scheduler Lambda.
type schedulerRequest struct {
	Services []config.ServiceConfig `json:"services"`
}

// schedulerResponse is received from the scheduler Lambda.
type schedulerResponse struct {
	NodeConfigs []config.NodeConfig `json:"node_configs"`
}

// invokeScheduler calls the scheduler Lambda with all services and returns
// per-instance NodeConfigs. Falls back gracefully if invocation fails.
func invokeScheduler(ctx context.Context, cfg Config, nodeConfigs []config.NodeConfig) ([]config.NodeConfig, error) {
	// Flatten per-node-type configs into a single service list.
	var services []config.ServiceConfig
	for _, nc := range nodeConfigs {
		services = append(services, nc.Services...)
	}

	req := schedulerRequest{Services: services}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling scheduler request: %w", err)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.SchedulerRegion))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := lambda.NewFromConfig(awsCfg)
	out, err := client.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(cfg.SchedulerLambdaARN),
		InvocationType: "RequestResponse",
		Payload:        payload,
	})
	if err != nil {
		return nil, fmt.Errorf("invoking scheduler Lambda: %w", err)
	}

	if out.FunctionError != nil {
		return nil, fmt.Errorf("scheduler Lambda returned error: %s: %s", *out.FunctionError, string(out.Payload))
	}

	var resp schedulerResponse
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		return nil, fmt.Errorf("unmarshaling scheduler response: %w", err)
	}

	return resp.NodeConfigs, nil
}
