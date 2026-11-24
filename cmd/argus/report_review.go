package main

import (
	"context"
	"fmt"
	"io"

	"github.com/abietic/argus/internal/reportreview"
	"github.com/abietic/argus/internal/store/local"
	contract "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const reportReviewUsage = `usage: argus report-review --store <absolute-dir> --request <absolute-json> --input-dir <absolute-dir>

Use a frozen report and profile evidence to commit a delivery-bound result.
Stdout contains one JSON result on success. Keep the same request and store for a retry.`

func runReportReview(ctx context.Context, arguments []string, stdout io.Writer) error {
	flags := newFlagSet("report-review")
	storePath := flags.String("store", "", "durable local review store")
	requestPath := flags.String("request", "", "frozen review request JSON")
	inputDir := flags.String("input-dir", "", "private staged input directory")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("%w\n%s", err, reportReviewUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("report-review accepts flags only\n%s", reportReviewUsage)
	}
	if _, err := validateExplicitControlStore(*storePath); err != nil {
		return err
	}
	if err := validateDescriptorPath("request", *requestPath); err != nil {
		return err
	}
	if err := validateDescriptorPath("input-dir", *inputDir); err != nil {
		return err
	}
	data, err := readRegularFileLimit(*requestPath, 16<<10)
	if err != nil {
		return fmt.Errorf("read report review request: %w", err)
	}
	request, err := contract.DecodeReportReviewRequest(data)
	if err != nil {
		return fmt.Errorf("decode report review request: %w", err)
	}
	store, err := local.Open(*storePath)
	if err != nil {
		return err
	}
	result, err := reportreview.Run(ctx, store, request, *inputDir)
	if err != nil {
		return err
	}
	n, err := stdout.Write(result)
	if err == nil && n != len(result) {
		return io.ErrShortWrite
	}
	return err
}
