package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"strings"
)

// Vision: when someone sends an image to Discord, the vision model reads it and
// describes what's there, and that description is folded into the turn so the
// normal tool loop (tcg price lookups, search) can act on it — e.g. snap a
// Pokémon card, ask "what's this worth?", get the rarebox-data price.

const maxVisionImageBytes = 5 << 20 // cap the base64 payload sent to the provider

// imageToDataURI fetches an image (SSRF-guarded) and returns an OpenAI
// image_url data URI. Discord CDN links are public but signed/expiring, so we
// inline the bytes rather than depend on the URL still resolving provider-side.
func imageToDataURI(url string) (string, error) {
	resp, err := ssrfClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	ct := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	if !strings.HasPrefix(ct, "image/") {
		return "", fmt.Errorf("not an image (%s)", ct)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVisionImageBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxVisionImageBytes {
		return "", fmt.Errorf("image too large (>5MB)")
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// describeImages runs the vision model over the attachments and returns a
// factual description tuned to the request. Best-effort: any failure returns
// "" (logged) so a bad image never blocks the text reply.
func (a *Agent) describeImages(request string, imageURLs []string) string {
	var uris []string
	for _, u := range imageURLs {
		du, err := imageToDataURI(u)
		if err != nil {
			log.Printf("vision: skip image: %v", err)
			continue
		}
		uris = append(uris, du)
	}
	if len(uris) == 0 {
		return ""
	}
	desc, err := a.llm.Vision(a.cfg.VisionModel, fmt.Sprintf(visionPrompt, request), uris)
	if err != nil {
		log.Printf("vision: %v", err)
		return ""
	}
	return strings.TrimSpace(desc)
}

const visionPrompt = `You are Vela's eyes. Describe exactly what is in the attached image(s), in the detail needed to help with this request: %q

If it's a trading card (Pokémon, Magic, Yu-Gi-Oh!, One Piece, Lorcana, etc.), report precisely: the card name, the set name and any set symbol or code, the collector number (e.g. "110/106"), the language (English or Japanese), and any visible rarity markers or special finish. If it's something else, describe what matters for the request. State ONLY what you can actually see — do not estimate prices, guess hidden details, or invent text. Be concise and factual.`

var imageExtSuffix = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}

// isImageName is a fallback for Discord attachments that arrive without a
// content-type — classify by extension.
func isImageName(name string) bool {
	n := strings.ToLower(name)
	for _, e := range imageExtSuffix {
		if strings.HasSuffix(n, e) {
			return true
		}
	}
	return false
}
