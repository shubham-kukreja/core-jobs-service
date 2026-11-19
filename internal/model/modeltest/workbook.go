// Package modeltest holds the MAAF v2 workbook's golden case, as shared on
// 2026-11-19: its scoring-sheet inputs, Small Cap vetoed, Capital Goods
// held, and its cached outputs on every scoring sheet and the Portfolio
// Rollup. It has no dependencies so any test package can use it.
package modeltest

// Line is one Portfolio Rollup line.
type Line struct {
	Item, Group string
	Weight      float64
}

// NeutralWeights are the scoring sheets' neutral weights (the balanced
// risk profile).
var NeutralWeights = map[string]string{
	"l1.equity": "0.55", "l1.debt": "0.35", "l1.metals": "0.10",
	"equity.large": "0.45", "equity.mid": "0.20", "equity.small": "0.15", "equity.intl": "0.20",
	"debt.liquid": "0.40", "debt.corporate": "0.35", "debt.gilt": "0.25",
	"metals.gold": "0.75", "metals.silver": "0.25",
}

// SectorsHeld is Portfolio Rollup B5 (one sector) with the sample name.
var SectorsHeld = []string{"sector.capgoods"}

// VetoTrigger is the trigger that vetoes Small Cap (Equity Segments D11).
const VetoTrigger = "trig.eps_collapse.equity.small"

// Scores are the scoring sheets' blue input cells, by bucket, in the
// config's signal order.
var Scores = map[string][]float64{
	// L1 Asset Classes B5:D9: valuation, macro, fundamentals, flows, momentum.
	"l1.equity": {45, 55, 55, 40, 60},
	"l1.debt":   {65, 60, 55, 55, 45},
	"l1.metals": {35, 60, 65, 30, 75},
	// Equity Segments B5:E9: valuation, relvalue, revisions, eps_growth_diff, margin_cycle.
	"equity.large": {55, 60, 50, 45, 55},
	"equity.mid":   {35, 30, 55, 60, 40},
	"equity.small": {25, 20, 45, 55, 30},
	"equity.intl":  {40, 45, 60, 65, 50},
	// Debt Buckets B5:D8: carry, rate_cycle, credit_spread, liquidity.
	"debt.liquid":    {55, 40, 50, 90},
	"debt.corporate": {60, 55, 45, 65},
	"debt.gilt":      {65, 70, 50, 75},
	// Precious Metals B5:C7: ratio, real_rates, industrial.
	"metals.gold":   {40, 65, 50},
	"metals.silver": {60, 55, 55},
	// Sector Satellite B5:E12: valuation, revisions, rel_momentum, cycle.
	"sector.banking":  {60, 55, 50, 55},
	"sector.it":       {55, 40, 35, 45},
	"sector.pharma":   {50, 65, 60, 55},
	"sector.auto":     {45, 55, 55, 60},
	"sector.capgoods": {60, 75, 80, 70},
	"sector.fmcg":     {55, 45, 40, 50},
	"sector.energy":   {60, 50, 45, 40},
	"sector.metals":   {55, 45, 50, 35},
}

// Outputs are the sheets' cached values: node → value.
var Outputs = map[string]float64{
	// L1 Asset Classes C15:G17.
	"l1.equity::composite": 50.5, "l1.equity::tilt": 0.002, "l1.equity::prelim": 0.5511, "l1.equity::weight": 0.5445114119,
	"l1.debt::composite": 57.5, "l1.debt::tilt": 0.03, "l1.debt::prelim": 0.3605, "l1.debt::weight": 0.3561900998,
	"l1.metals::composite": 51.25, "l1.metals::tilt": 0.005, "l1.metals::prelim": 0.1005, "l1.metals::weight": 0.09929848829,
	// Equity Segments C15:G18.
	"equity.large::composite": 53.5, "equity.large::tilt": 0.021, "equity.large::prelim": 0.45945, "equity.large::weight": 0.4661272732,
	"equity.mid::composite": 42.5, "equity.mid::tilt": -0.045, "equity.mid::prelim": 0.191, "equity.mid::weight": 0.1937758389,
	"equity.small::composite": 33.25, "equity.small::tilt": -0.1005, "equity.small::prelim": 0.134925, "equity.small::weight": 0.1368858904,
	"equity.intl::composite": 50.25, "equity.intl::tilt": 0.0015, "equity.intl::prelim": 0.2003, "equity.intl::weight": 0.2032109975,
	// Debt Buckets C14:G16.
	"debt.liquid::composite": 54.5, "debt.liquid::tilt": 0.027, "debt.liquid::prelim": 0.4108, "debt.liquid::weight": 0.3935902656,
	"debt.corporate::composite": 55.5, "debt.corporate::tilt": 0.033, "debt.corporate::prelim": 0.36155, "debt.corporate::weight": 0.3464035067,
	"debt.gilt::composite": 64.25, "debt.gilt::tilt": 0.0855, "debt.gilt::prelim": 0.271375, "debt.gilt::weight": 0.2600062277,
	// Precious Metals C13:G14.
	"metals.gold::composite": 51.25, "metals.gold::tilt": 0.00625, "metals.gold::prelim": 0.7546875, "metals.gold::weight": 0.7446808511,
	"metals.silver::composite": 57, "metals.silver::tilt": 0.035, "metals.silver::prelim": 0.25875, "metals.silver::weight": 0.2553191489,
	// Sector Satellite F5:F12.
	"sector.banking::composite": 55.25, "sector.it::composite": 44, "sector.pharma::composite": 57.75, "sector.auto::composite": 52.75,
	"sector.capgoods::composite": 71, "sector.fmcg::composite": 47.5, "sector.energy::composite": 50.25, "sector.metals::composite": 47.75,
	// Portfolio Rollup B6, C9:C13 and D9:D19.
	"l1.equity::sleeve":    0.075,
	"equity.large::within": 0.4311677277, "equity.mid::within": 0.179242651, "equity.small::within": 0.1266194486,
	"equity.intl::within": 0.1879701727, "sector.capgoods::within": 0.075,
	"equity.large::portfolio": 0.2347757482, "equity.mid::portfolio": 0.09759966896, "equity.small::portfolio": 0.06894573473,
	"equity.intl::portfolio": 0.1023519041, "sector.capgoods::portfolio": 0.04083835589,
	"debt.liquid::portfolio": 0.140192956, "debt.corporate::portfolio": 0.1233854996, "debt.gilt::portfolio": 0.09261164419,
	"metals.gold::portfolio": 0.07394568277, "metals.silver::portfolio": 0.02535280552,
}

// Rollup D9:D19 in sheet order (the empty "Sector 2" row is not a line).
var Lines = []Line{
	{"equity.large", "l1.equity", 0.2347757482}, {"equity.mid", "l1.equity", 0.09759966896},
	{"equity.small", "l1.equity", 0.06894573473}, {"equity.intl", "l1.equity", 0.1023519041},
	{"sector.capgoods", "l1.equity", 0.04083835589},
	{"debt.liquid", "l1.debt", 0.140192956}, {"debt.corporate", "l1.debt", 0.1233854996}, {"debt.gilt", "l1.debt", 0.09261164419},
	{"metals.gold", "l1.metals", 0.07394568277}, {"metals.silver", "l1.metals", 0.02535280552},
}
