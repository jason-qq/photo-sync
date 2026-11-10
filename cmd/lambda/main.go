// Command lambda runs the photo review app on AWS Lambda behind an API
// Gateway HTTP API. It serves exactly what cmd/api serves locally (built by
// app.New), backed by DynamoDB and S3.
//
// Configuration comes from the environment, set by deploy/template.yaml:
//
//	PHOTOSYNC_DYNAMODB_TABLE         DynamoDB table name
//	PHOTOSYNC_S3_BUCKET              S3 bucket for preview images
//	PHOTOSYNC_API_KEY_HASH_PARAM     SSM parameter: hex SHA-256 of the owner API key
//	PHOTOSYNC_SESSION_SECRET_PARAM   SSM parameter: session cookie signing secret
//
// Both SSM parameters are SecureStrings created by deploy/deploy.ps1. They
// are read once per cold start; after rotating one, new values take effect
// as Lambda replaces its execution environments (or after the next deploy).
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"photo-sync/internal/app"
	"photo-sync/internal/blob"
	"photo-sync/internal/lambdahttp"
	"photo-sync/internal/store"
)

func main() {
	// JSON logs are searchable as fields in CloudWatch Logs Insights.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// Setup runs once per execution environment (cold start), outside the
	// per-request handler, so its cost isn't paid on every request.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	handler, err := setup(ctx)
	cancel()
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
	lambda.Start(lambdahttp.Handler(handler))
}

func setup(ctx context.Context) (http.Handler, error) {
	table, err := requireEnv("PHOTOSYNC_DYNAMODB_TABLE")
	if err != nil {
		return nil, err
	}
	bucket, err := requireEnv("PHOTOSYNC_S3_BUCKET")
	if err != nil {
		return nil, err
	}
	hashParam, err := requireEnv("PHOTOSYNC_API_KEY_HASH_PARAM")
	if err != nil {
		return nil, err
	}
	secretParam, err := requireEnv("PHOTOSYNC_SESSION_SECRET_PARAM")
	if err != nil {
		return nil, err
	}

	// Region and credentials come from the Lambda environment (the
	// function's execution role).
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	params := ssm.NewFromConfig(cfg)

	hashHex, err := getSecureParam(ctx, params, hashParam)
	if err != nil {
		return nil, err
	}
	keyHash, err := hex.DecodeString(hashHex)
	if err != nil {
		return nil, fmt.Errorf("%s: not hex: %w", hashParam, err)
	}
	secret, err := getSecureParam(ctx, params, secretParam)
	if err != nil {
		return nil, err
	}

	return app.New(app.Config{
		APIKeyHash:    keyHash, // app/server validate length
		SessionSecret: []byte(secret),
		Store:         store.NewDynamo(dynamodb.NewFromConfig(cfg), table),
		Blobs:         blob.NewS3(blob.NewS3Client(cfg, ""), bucket),
	})
}

func getSecureParam(ctx context.Context, c *ssm.Client, name string) (string, error) {
	out, err := c.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("read SSM parameter %s: %w", name, err)
	}
	return aws.ToString(out.Parameter.Value), nil
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", errors.New(key + " must be set")
	}
	return v, nil
}
