package model

// Golden fixtures for the external (database) tests.
var (
	WorkbookScores  = workbookScores
	WorkbookOutputs = workbookOutputs
	// WorkbookPortfolio is Portfolio Rollup D9:D19 by line item.
	WorkbookPortfolio = func() map[string]float64 {
		m := map[string]float64{}
		for _, l := range workbookLines {
			m[l.item] = l.weight
		}
		return m
	}()
)
