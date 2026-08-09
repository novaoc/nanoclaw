package main

import (
	"encoding/json"
	"fmt"
	"html"
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

// cardHistory pulls a card's [epochDay,usd] series (best-tracked variant).
func cardHistory(game, set, number string, days int) ([]pricePoint, string, error) {
	game = strings.ToLower(strings.TrimSpace(game))
	set = strings.ToLower(strings.TrimSpace(set))
	var doc struct {
		Cards map[string]map[string][][]float64 `json:"cards"`
	}
	if err := rbGet(priceHistBase+"/"+game+"/"+set+".json", &doc); err != nil {
		return nil, "", fmt.Errorf("no price history for set %q", set)
	}
	variants := doc.Cards[rbNormNum(number)]
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
	if out := tc.saveArtifact(chartSlug(title)+".html", renderChartHTML(title, sub, pts)); strings.HasPrefix(out, "artifact error") {
		return out
	}
	first, last := pts[0].Price, pts[len(pts)-1].Price
	chg := 0.0
	if first != 0 {
		chg = (last - first) / first * 100
	}
	span := (pts[len(pts)-1].Ms - pts[0].Ms) / 86400000
	return fmt.Sprintf("Charted %s — %d points over ~%dd, $%.2f → $%.2f (%+.1f%%). Interactive chart is attached to your reply.",
		title, len(pts), span, first, last, chg)
}

func renderChartHTML(title, sub string, pts []pricePoint) string {
	arr := make([][2]float64, len(pts))
	for i, p := range pts {
		arr[i] = [2]float64{float64(p.Ms), p.Price}
	}
	data, _ := json.Marshal(arr)
	return strings.NewReplacer(
		"__TITLE__", html.EscapeString(title),
		"__SUB__", html.EscapeString(sub),
		"__DATA__", string(data),
	).Replace(chartTemplate)
}

const chartTemplate = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>__TITLE__</title>
<style>
:root{--bg:#0b0f14;--fg:#e6edf3;--mut:#7d8590;--grid:#1c2128;--accent:#58a6ff}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.4 -apple-system,Segoe UI,Roboto,sans-serif}
.wrap{max-width:880px;margin:0 auto;padding:20px}
h1{font-size:18px;margin:0 0 2px}.sub{color:var(--mut);font-size:12px;margin-bottom:14px}
.big{font-size:28px;font-weight:600}.chg{font-size:14px;font-weight:600;margin-left:8px}
.card{background:#0d1117;border:1px solid var(--grid);border-radius:12px;padding:16px}
svg{width:100%;height:auto;display:block;touch-action:none}
.axis{fill:var(--mut);font-size:11px}.gl{stroke:var(--grid);stroke-width:1}
#tip{position:fixed;pointer-events:none;background:#161b22;border:1px solid var(--grid);border-radius:8px;padding:6px 9px;font-size:12px;opacity:0;transition:opacity .08s;white-space:nowrap;z-index:9}
#tip b{color:var(--accent)}.foot{color:var(--mut);font-size:11px;margin-top:10px;text-align:right}
</style></head><body><div class="wrap">
<h1>__TITLE__</h1><div class="sub">__SUB__ · <span id="range"></span></div>
<div class="card"><div><span class="big" id="last"></span><span class="chg" id="chg"></span></div>
<svg id="chart" viewBox="0 0 880 360"></svg></div>
<div class="foot">nanoclaw · historical prices in USD · not financial advice</div>
</div><div id="tip"></div>
<script>
const DATA=__DATA__;
const W=880,H=360,PL=58,PR=16,PT=18,PB=26;
const svg=document.getElementById('chart'),tip=document.getElementById('tip');
const xs=DATA.map(d=>d[0]),ys=DATA.map(d=>d[1]);
const x0=Math.min(...xs),x1=Math.max(...xs),y0=Math.min(...ys),y1=Math.max(...ys);
const pad=(y1-y0)*0.08||Math.abs(y1)*0.08||1,ymin=y0-pad,ymax=y1+pad;
const X=v=>PL+(v-x0)/((x1-x0)||1)*(W-PL-PR);
const Y=v=>PT+(1-(v-ymin)/((ymax-ymin)||1))*(H-PT-PB);
const money=v=>'$'+v.toLocaleString(undefined,{maximumFractionDigits:v<10?4:2});
const day=ms=>new Date(ms).toLocaleDateString(undefined,{month:'short',day:'numeric'});
const up=ys[ys.length-1]>=ys[0],col=up?'#3fb950':'#f85149';
let line='';DATA.forEach((d,i)=>line+=(i?'L':'M')+X(d[0]).toFixed(1)+' '+Y(d[1]).toFixed(1)+' ');
let g='';
for(let i=0;i<=4;i++){const v=ymin+(ymax-ymin)*i/4,yy=Y(v);
 g+='<line class="gl" x1="'+PL+'" y1="'+yy+'" x2="'+(W-PR)+'" y2="'+yy+'"/><text class="axis" x="'+(PL-6)+'" y="'+(yy+3)+'" text-anchor="end">'+money(v)+'</text>';}
for(let i=0;i<=3;i++){const v=x0+(x1-x0)*i/3;g+='<text class="axis" x="'+X(v)+'" y="'+(H-8)+'" text-anchor="middle">'+day(v)+'</text>';}
svg.innerHTML=g+
 '<defs><linearGradient id="f" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="'+col+'" stop-opacity=".25"/><stop offset="1" stop-color="'+col+'" stop-opacity="0"/></linearGradient></defs>'+
 '<path d="'+line+'L'+X(x1)+' '+Y(ymin)+' L'+X(x0)+' '+Y(ymin)+' Z" fill="url(#f)"/>'+
 '<path d="'+line+'" fill="none" stroke="'+col+'" stroke-width="2"/>'+
 '<line id="cx" y1="'+PT+'" y2="'+(H-PB)+'" stroke="'+col+'" stroke-width="1" opacity="0"/>'+
 '<circle id="dot" r="4" fill="'+col+'" opacity="0"/>';
const last=ys[ys.length-1],chg=(last-ys[0])/(ys[0]||1)*100;
document.getElementById('last').textContent=money(last);
const ce=document.getElementById('chg');ce.textContent=(chg>=0?'▲ ':'▼ ')+chg.toFixed(1)+'%';ce.style.color=col;
document.getElementById('range').textContent=day(x0)+' – '+day(x1)+' · '+DATA.length+' pts';
const cx=document.getElementById('cx'),dot=document.getElementById('dot');
function mv(ev){const r=svg.getBoundingClientRect(),px=(ev.clientX-r.left)/r.width*W;
 let bi=0,bd=1e18;DATA.forEach((d,i)=>{const q=Math.abs(X(d[0])-px);if(q<bd){bd=q;bi=i}});
 const d=DATA[bi];cx.setAttribute('x1',X(d[0]));cx.setAttribute('x2',X(d[0]));cx.setAttribute('opacity','.5');
 dot.setAttribute('cx',X(d[0]));dot.setAttribute('cy',Y(d[1]));dot.setAttribute('opacity','1');
 tip.style.opacity=1;tip.style.left=Math.min(ev.clientX+12,innerWidth-120)+'px';tip.style.top=(ev.clientY-10)+'px';
 tip.innerHTML='<b>'+money(d[1])+'</b><br>'+new Date(d[0]).toLocaleDateString();}
function lv(){cx.setAttribute('opacity','0');dot.setAttribute('opacity','0');tip.style.opacity=0;}
svg.addEventListener('mousemove',mv);svg.addEventListener('mouseleave',lv);
svg.addEventListener('touchmove',e=>{mv(e.touches[0]);e.preventDefault();},{passive:false});
</script></body></html>`
