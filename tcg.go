package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// TCG lookup over rarebox-data (github.com/novaoc/rarebox-data): the open,
// daily-refreshed catalog + price dataset. Cards, sets, and market prices for
// Pokémon (EN/JP), Magic, Yu-Gi-Oh!, Lorcana, One Piece (EN/JP), Riftbound.
// Read-only public JSON — no key, CORS-open, docs at docs.rarebox.io/data.

const rbData = "https://raw.githubusercontent.com/novaoc/rarebox-data/main"

var tcgGames = map[string]bool{
	"pokemon": true, "pokemon-ja": true, "mtg": true, "yugioh": true,
	"lorcana": true, "one-piece": true, "one-piece-ja": true, "riftbound": true,
}

var numLetterPrefix = regexp.MustCompile(`^([a-z]+)0*(\d.*)$`)

// rbNormNum mirrors the dataset's number normalization (SCHEMA.md): lowercase,
// drop the "/total" suffix and leading zeros; "001/217"→"1", "TG07"→"tg7".
func rbNormNum(s string) string {
	n := strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[:i]
	}
	trimmed := strings.TrimLeft(n, "0")
	if trimmed == "" {
		return "0"
	}
	n = trimmed
	if m := numLetterPrefix.FindStringSubmatch(n); m != nil {
		n = m[1] + m[2]
	}
	return n
}

func rbGet(url string, v any) error {
	resp, err := ssrfClient.Get(url)
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

type rbSet struct {
	ID, Name, Series, ReleaseDate string
	Total                         int
}
type rbCard struct {
	ID, Name, Number, Rarity, Image string
	TcgplayerID                     string `json:"tcgplayerId"` // riftbound price/history key
	Set                             struct{ ID, Name string }
}

// rbPrices loads a game's price map (key → USD). Values are usually a flat
// float, but some games (riftbound) use a {normal, foil} object — parse both.
// Best-effort: prices are optional, so a miss returns an empty map.
func rbPrices(game string) map[string]float64 {
	var priced struct {
		Prices map[string]json.RawMessage `json:"prices"`
	}
	_ = rbGet(rbData+"/prices/"+game+"/latest.json", &priced)
	out := make(map[string]float64, len(priced.Prices))
	for k, v := range priced.Prices {
		if p, ok := parsePriceVal(v); ok {
			out[k] = p
		}
	}
	return out
}

// parsePriceVal reads a flat number, or a {normal, foil} object (normal wins).
func parsePriceVal(v json.RawMessage) (float64, bool) {
	var f float64
	if json.Unmarshal(v, &f) == nil {
		return f, f > 0
	}
	var o struct{ Normal, Foil *float64 }
	if json.Unmarshal(v, &o) == nil {
		if o.Normal != nil && *o.Normal > 0 {
			return *o.Normal, true
		}
		if o.Foil != nil && *o.Foil > 0 {
			return *o.Foil, true
		}
	}
	return 0, false
}

// priceStr formats a card's market price if the dataset has one.
func priceStr(prices map[string]float64, set, number string) string {
	if p, ok := prices[strings.ToLower(set)+"-"+rbNormNum(number)]; ok {
		return fmt.Sprintf(" — $%.2f", p)
	}
	return ""
}

// cardPrice formats a card's price using the per-game key scheme rarebox uses:
// most games key by set+number, but One Piece keys by card id (OP01-024).
func cardPrice(prices map[string]float64, game, set string, c rbCard) string {
	key := strings.ToLower(set) + "-" + rbNormNum(c.Number)
	switch {
	case game == "one-piece" || game == "one-piece-ja":
		key = strings.ToUpper(c.ID)
	case game == "riftbound" && c.TcgplayerID != "":
		key = c.TcgplayerID
	}
	if p, ok := prices[key]; ok {
		return fmt.Sprintf(" — $%.2f", p)
	}
	return ""
}

var punctRe = regexp.MustCompile(`[^a-z0-9]+`)

func looseNorm(s string) string { return punctRe.ReplaceAllString(strings.ToLower(s), "") }

// looseContains matches a card name ignoring case, spaces, and punctuation, so
// "Monkey D. Luffy" finds "Monkey.D.Luffy" and "Ahri signature" isn't needed
// exact. Empty query matches nothing here (callers handle the no-filter case).
func looseContains(name, query string) bool {
	qn := looseNorm(query)
	return qn != "" && strings.Contains(looseNorm(name), qn)
}

// jaHint nudges the model to search in English when a lookup misses — the
// dataset indexes every game (Japanese sets included) under ENGLISH card names,
// so a Japanese query never matches and retrying it just burns tool budget.
func jaHint(game, query string) string {
	if hasNonASCII(query) || strings.HasSuffix(game, "-ja") {
		return " NOTE: card names here are ENGLISH for every game, Japanese sets too. If you searched a Japanese name, translate it to English and retry ONCE (e.g. メガリザードンX ex → \"Mega Charizard X\"); do not repeat the same query."
	}
	return ""
}

// pokeResolve is the EN-Pokémon fallback when the recent-set scan misses an
// older card: pokemontcg.io indexes every set and returns rarebox-compatible
// set ids, so we hand back the correct set/number (+ the rarebox price when we
// have it) instead of the model guessing a wrong set code.
func pokeResolve(game, q string, prices map[string]float64) string {
	var d struct {
		Data []struct {
			Name, Number, Rarity string
			Set                  struct{ ID, Name string }
			Images               struct{ Small string }
		}
	}
	u := "https://api.pokemontcg.io/v2/cards?pageSize=20&q=" + url.QueryEscape(`name:"`+q+`"`)
	if err := fetchJSON(u, &d); err != nil || len(d.Data) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s cards matching %q (searched all sets):\n", game, q)
	for i, c := range d.Data {
		if i >= 20 {
			break
		}
		cardLine(&b, rbCard{Name: c.Name, Number: c.Number, Rarity: c.Rarity, Image: c.Images.Small},
			priceStr(prices, c.Set.ID, c.Number)+" (set "+c.Set.ID+" — "+c.Set.Name+")")
	}
	b.WriteString("(the set ids shown are the ones to pass to tcg or price_chart.)")
	return clip(b.String(), 6000)
}

func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

func cardLine(b *strings.Builder, c rbCard, price string) {
	rar := ""
	if c.Rarity != "" {
		rar = " [" + c.Rarity + "]"
	}
	fmt.Fprintf(b, "• %s #%s%s%s\n  %s\n", c.Name, c.Number, rar, price, c.Image)
}

// tcgLookup: no set + query → search cards by NAME across the most recent sets
// (so the caller never has to guess a set id) and, if the query also names a
// set, list those sets. With a set id → cards in it (filtered by query). Each
// card carries number, rarity, market price (merged from prices/), and the
// image URL (attachable via attach_image).
func tcgLookup(game, set, query string) string {
	game = strings.ToLower(strings.TrimSpace(game))
	if !tcgGames[game] {
		return "tcg error: unknown game. Options: pokemon, pokemon-ja, mtg, yugioh, lorcana, one-piece, one-piece-ja, riftbound"
	}
	q := strings.ToLower(strings.TrimSpace(query))
	set = strings.TrimSpace(set)

	if set == "" {
		var sets []rbSet
		if err := rbGet(rbData+"/catalog/"+game+"/sets.json", &sets); err != nil {
			return "tcg error: " + err.Error()
		}
		var setMatches []rbSet
		if q != "" {
			for _, s := range sets {
				if strings.Contains(strings.ToLower(s.Name), q) || strings.Contains(strings.ToLower(s.ID), q) {
					setMatches = append(setMatches, s)
				}
			}
			// Query didn't name a set → it's almost certainly a card. Search
			// cards by name across the newest sets instead of making the caller
			// guess a set id (the JP-secret-rare trap).
			if len(setMatches) == 0 {
				return tcgCardSearch(game, q, sets)
			}
			sets = setMatches
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s sets", game)
		if q != "" {
			fmt.Fprintf(&b, " matching %q", query)
		}
		b.WriteString(":\n")
		for i, s := range sets {
			if i >= 25 {
				break
			}
			fmt.Fprintf(&b, "• %s — %s (%d cards, %s)\n", s.ID, s.Name, s.Total, s.ReleaseDate)
		}
		b.WriteString("Look up a set's cards by passing its id as `set` — or just pass the card name as `query` to search across sets.")
		return clip(b.String(), 6000)
	}

	var cards []rbCard
	if err := rbGet(rbData+"/catalog/"+game+"/sets/"+set+".json", &cards); err != nil {
		return "tcg error: couldn't load set " + set + " (" + err.Error() + "). List sets first (omit `set`), or pass the card name as `query` to search across sets."
	}
	prices := rbPrices(game)

	var b strings.Builder
	fmt.Fprintf(&b, "%s / %s", game, set)
	if q != "" {
		fmt.Fprintf(&b, " matching %q", query)
	}
	b.WriteString(":\n")
	n := 0
	for _, c := range cards {
		if q != "" && !looseContains(c.Name, q) {
			continue
		}
		cardLine(&b, c, cardPrice(prices, game, set, c))
		if n++; n >= 15 {
			break
		}
	}
	if n == 0 {
		return "no cards matched in " + set + " — try a different name, or omit the query to list the set." + jaHint(game, query)
	}
	b.WriteString("(image URLs shown — offer to attach one if the user wants to see it. When several same-named cards exist and rarity is blank, the higher-priced ones are the chase/secret-rare versions.)")
	return clip(b.String(), 6000)
}

// tcgCardSearch scans the most recent sets for cards whose name matches q, so a
// caller can find a card (and its price) without knowing which set it's in.
// Bounded to the newest sets to keep it to a handful of fetches on the board.
func tcgCardSearch(game, q string, sets []rbSet) string {
	sort.SliceStable(sets, func(i, j int) bool { return sets[i].ReleaseDate > sets[j].ReleaseDate })
	prices := rbPrices(game)

	const maxSets = 24
	var b strings.Builder
	n, scanned := 0, 0
	for _, s := range sets {
		if scanned >= maxSets || n >= 20 {
			break
		}
		scanned++
		var cards []rbCard
		if err := rbGet(rbData+"/catalog/"+game+"/sets/"+s.ID+".json", &cards); err != nil {
			continue
		}
		for _, c := range cards {
			if !looseContains(c.Name, q) {
				continue
			}
			cardLine(&b, c, cardPrice(prices, game, s.ID, c)+" (set "+s.ID+" — "+s.Name+")")
			if n++; n >= 20 {
				break
			}
		}
	}
	if n == 0 {
		// Recent-set scan missed — likely an older card. For EN Pokémon, resolve
		// it across ALL sets via pokemontcg.io (which uses the same set ids as
		// rarebox), so the model gets the right set/number instead of guessing a
		// wrong code and bailing to the web.
		if game == "pokemon" {
			if alt := pokeResolve(game, q, prices); alt != "" {
				return alt
			}
		}
		return fmt.Sprintf("no cards named %q in the %d most recent %s sets. For an older card, pass its set id as `set`; to browse sets, omit `query`.", q, scanned, game) + jaHint(game, q)
	}
	head := fmt.Sprintf("%s cards matching %q (searched the %d newest sets):\n", game, q, scanned)
	tail := "\n(image URLs shown — offer to attach one if they want to see it. When same-named cards repeat with blank rarity, the higher-priced ones are the chase/secret-rare versions.)"
	return clip(head+b.String()+tail, 6000)
}
