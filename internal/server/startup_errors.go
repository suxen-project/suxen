package server

import (
	"github.com/suxen-project/suxen/internal/provision"
	"github.com/suxen-project/suxen/internal/startup"
)

func classifyProvisionError(err error) error {
	if provision.IsValidationFailure(err) {
		return err
	}
	return startup.Retry(err)
}

func classifyProvisionReport(report provision.Report) error {
	err := report.Failure()
	if report.HasValidationFailure() {
		return err
	}
	return startup.Retry(err)
}
