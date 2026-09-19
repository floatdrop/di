//go:build ignore

// Command og renders the repository's Open Graph preview card.
//
// The card is 1280x640: GitHub's recommended size for a repository's social
// preview, and a shape link unfurlers crop from without losing anything, so
// one file serves both the repository setting and the site's og:image.
//
// The gopher is docs/assets/logo.svg, read here and placed as a group, the way
// site/src/components/Logo.tsx reads it into the page, so a redrawn logo
// reaches the card by re-running this rather than by being copied into it.
// The words are the site's: its hero title and its install line.
//
// Usage, from the repository root:
//
//	go run scripts/og.go            # writes docs/assets/og.svg and og.png
//	go run scripts/og.go -svg-only  # the vector alone, no rasteriser needed
//
// The PNG is rasterised by resvg or rsvg-convert, whichever is on PATH, and
// the text is set in whatever the system resolves the families below to. Both
// files are committed, so that only matters when the card is regenerated: if
// the type in a fresh PNG shifts, it is the font that changed, not the card.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// The palette is the logo's own, so the card cannot drift from the drawing in
// the one way reading the file cannot catch.
const (
	paper = "#FFFDF8" // the gopher's white, and the ground here
	ink   = "#27343B" // its outline, and every piece of type
	coral = "#F58068" // the cable and the connectors
	cyan  = "#79D6E8" // the fur
)

// The faces the site asks for, in the same order, minus the ui- prefixed
// families no rasteriser resolves.
const (
	sans = "system-ui, -apple-system, 'Helvetica Neue', 'Segoe UI', Roboto, Arial, sans-serif"
	mono = "SFMono-Regular, Menlo, Consolas, 'Liberation Mono', monospace"
)

func main() {
	svgOnly := flag.Bool("svg-only", false, "write the SVG and skip rasterising")
	flag.Parse()

	logo, err := os.ReadFile("docs/assets/logo.svg")
	if err != nil {
		die(err)
	}
	card, err := compose(string(logo))
	if err != nil {
		die(err)
	}
	if err := os.WriteFile("docs/assets/og.svg", []byte(card), 0o644); err != nil {
		die(err)
	}
	fmt.Println("docs/assets/og.svg")
	if *svgOnly {
		return
	}
	if err := rasterise("docs/assets/og.svg", "docs/assets/og.png"); err != nil {
		die(err)
	}
	fmt.Println("docs/assets/og.png")
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "og:", err)
	os.Exit(1)
}

var (
	openTag  = regexp.MustCompile(`(?s)^.*?<svg[^>]*>`)
	closeTag = regexp.MustCompile(`(?s)</svg>\s*$`)
	titleTag = regexp.MustCompile(`(?s)<title[^>]*>.*?</title>`)
	descTag  = regexp.MustCompile(`(?s)<desc[^>]*>.*?</desc>`)
)

// drawing is the logo's paths without its outer element: its accessible name
// belongs to the file, and the card has a name of its own.
func drawing(logo string) (string, error) {
	inner := descTag.ReplaceAllString(titleTag.ReplaceAllString(closeTag.ReplaceAllString(openTag.ReplaceAllString(logo, ""), ""), ""), "")
	if inner == logo || !strings.Contains(inner, "<g ") {
		return "", fmt.Errorf("docs/assets/logo.svg is not the svg element with a group in it that this expects")
	}
	return strings.TrimSpace(inner), nil
}

// The card, laid out in its own pixels. The logo's 400x400 frame is padded on
// every side, so it is placed a little larger than the height it is given and
// nudged up: what should sit level with the type is the gopher, not its box.
const (
	width  = 1280
	height = 640

	logoScale = 1.28
	logoX     = 30.0
	logoY     = 57.0

	textX = 572.0 // the type's left edge, clear of the cable's widest loop
)

func compose(logo string) (string, error) {
	inner, err := drawing(logo)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&sb, format+"\n", args...) }

	p(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title description">`, width, height, width, height)
	p(`  <title id="title">golang.yandex/di — dependency injection for Go</title>`)
	p(`  <desc id="description">The di gopher joining two cable connectors, beside the words: di, dependency injection for Go, built on generic methods, and the command go get golang.yandex/di.</desc>`)
	p(`  <rect width="%d" height="%d" fill="%s"/>`, width, height, paper)

	// The cable runs off the bottom of the card as a rule the width of the
	// page, coral over cyan: the drawing's own two colours, in its own order.
	p(`  <rect x="0" y="%d" width="%d" height="10" fill="%s"/>`, height-10, width, cyan)
	p(`  <rect x="0" y="%d" width="%d" height="10" fill="%s"/>`, height-10, int(textX), coral)

	p(`  <g transform="translate(%.0f %.0f) scale(%g)">`, logoX, logoY, logoScale)
	p(`%s`, indent(inner, "    "))
	p(`  </g>`)

	p(`  <g fill="%s" font-family="%s">`, ink, sans)
	p(`    <text x="%.0f" y="242" font-size="150" font-weight="700" letter-spacing="-4">di</text>`, textX)
	p(`    <text x="%.0f" y="336" font-size="44" font-weight="500" opacity="0.88">Dependency injection for Go,</text>`, textX)
	p(`    <text x="%.0f" y="394" font-size="44" font-weight="500" opacity="0.88">built on generic methods.</text>`, textX)
	p(`  </g>`)

	// The install line, in the box the site puts it in.
	p(`  <rect x="%.0f" y="452" width="596" height="72" rx="14" fill="none" stroke="%s" stroke-width="2.5" opacity="0.28"/>`, textX, ink)
	p(`  <text x="%.0f" y="497" font-family="%s" font-size="29" fill="%s" opacity="0.92">go get golang.yandex/di</text>`, textX+28, mono, ink)

	p(`</svg>`)
	return sb.String(), nil
}

func indent(s, with string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			lines[i] = with + line
		}
	}
	return strings.Join(lines, "\n")
}

// rasterise renders the card with whichever converter is installed. Both read
// the SVG's own width and height, so neither is told a size.
func rasterise(in, out string) error {
	converters := [][]string{
		{"resvg", in, out},
		{"rsvg-convert", "-o", out, in},
	}
	for _, argv := range converters {
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return fmt.Errorf("no rasteriser: install resvg or librsvg (brew install resvg), or pass -svg-only")
}
