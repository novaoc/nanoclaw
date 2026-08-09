package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Price charts: fetch a historical price series (cards → rarebox-price-history,
// crypto → CoinGecko, stocks → Yahoo Finance, all keyless) and render a
// self-contained INTERACTIVE HTML chart (hover for date+price) as an artifact.
// This is neutral data visualization — no trading, no advice.

type pricePoint struct {
	Ms    int64
	Price float64
}

const priceHistBase = "https://raw.githubusercontent.com/novaoc/rarebox-price-history/main/data"

// fetchJSON GETs a JSON API with a browser UA (CoinGecko/Yahoo gate on UA),
// SSRF-guarded like every other outbound call.
func fetchJSON(u string, v any) error {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	resp, err := ssrfClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// lastDays trims an ascending-by-time series to the trailing window.
func lastDays(pts []pricePoint, days int) []pricePoint {
	if days <= 0 || len(pts) < 2 {
		return pts
	}
	cutoff := pts[len(pts)-1].Ms - int64(days)*86400000
	var out []pricePoint
	for _, p := range pts {
		if p.Ms >= cutoff {
			out = append(out, p)
		}
	}
	if len(out) < 2 {
		return pts
	}
	return out
}

// cardHistory pulls a card's [epochDay,usd] series (best-tracked variant). The
// history file name and the per-card key are game-specific.
func cardHistory(game, set, number string, days int) ([]pricePoint, string, error) {
	game = strings.ToLower(strings.TrimSpace(game))
	set = strings.TrimSpace(set)
	fileSet := strings.ToLower(set)
	key := rbNormNum(number)
	if game == "one-piece" || game == "one-piece-ja" {
		// files are OP-01.json; keys are OP01-003 (set sans dash + 3-digit num)
		fileSet = strings.ToUpper(set)
		n := rbNormNum(number)
		for len(n) < 3 {
			n = "0" + n
		}
		key = strings.ToUpper(strings.ReplaceAll(set, "-", "")) + "-" + n
	}
	var doc struct {
		Cards map[string]map[string][][]float64 `json:"cards"`
	}
	if game == "riftbound" {
		// history keys by TCGplayer productId — resolve it from the catalog by
		// collector number, picking the printing that actually has history.
		fileSet = strings.ToUpper(set)
		if err := rbGet(priceHistBase+"/riftbound/"+fileSet+".json", &doc); err != nil {
			return nil, "", fmt.Errorf("no price history for set %q", set)
		}
		var cat []rbCard
		_ = rbGet(rbData+"/catalog/riftbound/sets/"+fileSet+".json", &cat)
		n := rbNormNum(number)
		bestLen := 0
		for _, c := range cat {
			if rbNormNum(c.Number) != n || c.TcgplayerID == "" {
				continue
			}
			tot := 0
			for _, s := range doc.Cards[c.TcgplayerID] {
				tot += len(s)
			}
			if tot > bestLen {
				bestLen, key = tot, c.TcgplayerID
			}
		}
		if bestLen == 0 {
			return nil, "", fmt.Errorf("no price history for riftbound #%s", number)
		}
	} else if err := rbGet(priceHistBase+"/"+game+"/"+fileSet+".json", &doc); err != nil {
		return nil, "", fmt.Errorf("no price history for set %q", set)
	}
	variants := doc.Cards[key]
	if len(variants) == 0 {
		return nil, "", fmt.Errorf("no price history for #%s in %s", number, set)
	}
	var best [][]float64
	var bestVar string
	for name, s := range variants {
		if len(s) > len(best) {
			best, bestVar = s, name
		}
	}
	var pts []pricePoint
	for _, p := range best {
		if len(p) >= 2 {
			pts = append(pts, pricePoint{Ms: int64(p[0]) * 86400000, Price: p[1]})
		}
	}
	return lastDays(pts, days), bestVar, nil
}

// cryptoHistory resolves a name/symbol to a CoinGecko id and pulls daily USD.
func cryptoHistory(query string, days int) ([]pricePoint, string, error) {
	var sr struct {
		Coins []struct{ ID, Name string }
	}
	if err := fetchJSON("https://api.coingecko.com/api/v3/search?query="+url.QueryEscape(query), &sr); err != nil {
		return nil, "", err
	}
	if len(sr.Coins) == 0 {
		return nil, "", fmt.Errorf("no token matching %q", query)
	}
	id, name := sr.Coins[0].ID, sr.Coins[0].Name
	var d struct {
		Prices [][]float64 `json:"prices"`
	}
	u := fmt.Sprintf("https://api.coingecko.com/api/v3/coins/%s/market_chart?vs_currency=usd&days=%d&interval=daily", url.PathEscape(id), days)
	if err := fetchJSON(u, &d); err != nil {
		return nil, "", err
	}
	var pts []pricePoint
	for _, p := range d.Prices {
		if len(p) >= 2 {
			pts = append(pts, pricePoint{Ms: int64(p[0]), Price: p[1]})
		}
	}
	return pts, name, nil
}

// stockHistory pulls daily closes from Yahoo Finance.
func stockHistory(symbol string, days int) ([]pricePoint, string, error) {
	rng := "1mo"
	switch {
	case days > 186:
		rng = "1y"
	case days > 93:
		rng = "6mo"
	case days > 31:
		rng = "3mo"
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	u := "https://query1.finance.yahoo.com/v8/finance/chart/" + url.PathEscape(sym) + "?range=" + rng + "&interval=1d"
	var y struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64
				Indicators struct {
					Quote []struct{ Close []float64 }
				}
			}
		}
	}
	if err := fetchJSON(u, &y); err != nil {
		return nil, "", err
	}
	if len(y.Chart.Result) == 0 || len(y.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, "", fmt.Errorf("no data for %s", sym)
	}
	r := y.Chart.Result[0]
	cl := r.Indicators.Quote[0].Close
	var pts []pricePoint
	for i, ts := range r.Timestamp {
		if i < len(cl) && cl[i] > 0 {
			pts = append(pts, pricePoint{Ms: ts * 1000, Price: cl[i]})
		}
	}
	return lastDays(pts, days), sym, nil
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func chartSlug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		s = "chart"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// priceChart is the tool entry point.
func (tc *ToolCtx) priceChart(a toolArgs) string {
	kind := strings.ToLower(strings.TrimSpace(a.Kind))
	days := a.Days
	var pts []pricePoint
	var title, sub string
	var err error
	switch kind {
	case "card":
		// History for these games is keyed by collector number; others (one-piece
		// by card id, riftbound by product id, yugioh by set code) don't align, so
		// fail fast with a clear message instead of letting the model retry.
		chartable := map[string]bool{"pokemon": true, "pokemon-ja": true, "mtg": true, "lorcana": true, "one-piece": true, "one-piece-ja": true, "riftbound": true}
		if !chartable[strings.ToLower(strings.TrimSpace(a.Game))] {
			return "chart error: price charts aren't available for " + a.Game + " cards yet (its history isn't keyed by collector number). Give the current price from tcg instead — don't retry the chart."
		}
		if a.Game == "" || a.Set == "" || a.Number == "" {
			return "chart error: for a card I need game, set, and number — look them up with tcg first."
		}
		if days == 0 {
			days = 90
		}
		var variant string
		pts, variant, err = cardHistory(a.Game, a.Set, a.Number, days)
		title = strings.TrimSpace(a.Query)
		if title == "" {
			title = fmt.Sprintf("%s %s #%s", a.Game, strings.ToUpper(a.Set), a.Number)
		}
		sub = fmt.Sprintf("%s %s #%s · %s", a.Game, strings.ToUpper(a.Set), a.Number, variant)
	case "crypto":
		if a.Query == "" {
			return "chart error: crypto needs a name or symbol (e.g. bitcoin)."
		}
		if days == 0 {
			days = 30
		}
		pts, title, err = cryptoHistory(a.Query, days)
		sub = "CoinGecko · USD"
	case "stock":
		sym := a.Symbol
		if sym == "" {
			sym = a.Query
		}
		if sym == "" {
			return "chart error: stock needs a ticker (e.g. AAPL)."
		}
		if days == 0 {
			days = 30
		}
		pts, title, err = stockHistory(sym, days)
		sub = "Yahoo Finance · USD"
	default:
		return "chart error: kind must be card, stock, or crypto."
	}
	if err != nil {
		return "chart error: " + err.Error()
	}
	if len(pts) < 2 {
		return "chart error: not enough price history to chart that yet."
	}
	if out := tc.saveArtifact(chartSlug(title)+".png", string(renderChartPNG(title, sub, pts))); strings.HasPrefix(out, "artifact error") {
		return out
	}
	first, last := pts[0].Price, pts[len(pts)-1].Price
	chg := 0.0
	if first != 0 {
		chg = (last - first) / first * 100
	}
	span := (pts[len(pts)-1].Ms - pts[0].Ms) / 86400000
	return fmt.Sprintf("Charted %s — %d points over ~%dd, $%.2f → $%.2f (%+.1f%%). Chart image is attached to your reply.",
		title, len(pts), span, first, last, chg)
}
