// Package quality finds stale, missing and suspicious data: the analyst's
// to-do list while data entry is manual (the quality.check job).
package quality

import (
	"context"

	"cloud.google.com/go/civil"
)

// Check names.
const (
	CheckStale               = "stale"
	CheckMissingScores       = "missing_scores"
	CheckInsufficientHistory = "insufficient_history"
	CheckOutliers            = "outliers"
	CheckBadCategories       = "bad_categories"
)

// Request selects what to check. Empty slices mean all.
type Request struct {
	AsOf   civil.Date
	Series []string
	Checks []string
}

// Stale is a series with no value for its expected period.
type Stale struct {
	Series     string     `json:"series"`
	LastObs    civil.Date `json:"last_obs"`
	ExpectedBy civil.Date `json:"expected_by"`
}

// Outlier is a value with |z| above the threshold.
type Outlier struct {
	Series  string     `json:"series"`
	ObsDate civil.Date `json:"obs_date"`
	Value   float64    `json:"value"`
	Z       float64    `json:"z"`
}

// InsufficientHistory is a percentile series below its minimum observations.
type InsufficientHistory struct {
	Series string `json:"series"`
	Obs    int    `json:"obs"`
	Min    int    `json:"min"`
}

// BadCategory is a categorical value outside the allowed set.
type BadCategory struct {
	Series  string     `json:"series"`
	ObsDate civil.Date `json:"obs_date"`
	Value   string     `json:"value"`
}

// Report is the result of a quality check.
type Report struct {
	AsOf                civil.Date            `json:"as_of"`
	Stale               []Stale               `json:"stale"`
	Outliers            []Outlier             `json:"outliers"`
	InsufficientHistory []InsufficientHistory `json:"insufficient_history"`
	MissingScores       []string              `json:"missing_scores"`
	BadCategories       []BadCategory         `json:"bad_categories"`
}

// Checker runs checks.
type Checker interface {
	Check(ctx context.Context, r Request) (Report, error)
}

// OutlierZ is the |z| above which a value is flagged.
const OutlierZ = 4.0
