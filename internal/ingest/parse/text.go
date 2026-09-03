package parse

import "strings"

// textParser handles text/plain: normalise line endings and split on blank lines
// into paragraph blocks (SPEC-05 §2 "Go passthrough"). Plain text carries no
// headings or title in its body, so Title stays empty — it comes from source
// metadata.
type textParser struct{}

func (textParser) Parse(data []byte) (Normalised, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var blocks []Block
	for _, para := range strings.Split(text, "\n\n") {
		if t := collapseWS(para); t != "" {
			blocks = append(blocks, Block{Type: Paragraph, Text: t})
		}
	}
	return Normalised{Blocks: blocks}, nil
}
