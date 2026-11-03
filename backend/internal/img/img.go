// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Package img draws product images as SVG, one illustration per category, tinted
// with a per-brand color. The catalog is fictional, so there are no real product
// photos; generated SVGs keep the repo small, license-clean and work offline.
// In production this would be a CDN URL stored on the product row in Postgres.
package img

import (
	"fmt"
	"hash/fnv"
)

// palette: main color + soft background, readable in light and dark themes.
var palette = [][2]string{
	{"#e2552d", "#fde9e2"}, {"#2563eb", "#e3ecfd"}, {"#0f766e", "#dcf3ef"}, {"#7c3aed", "#ede5fd"},
	{"#b45309", "#fbecd8"}, {"#be185d", "#fbe3ee"}, {"#334155", "#e6e9ee"}, {"#15803d", "#e0f3e6"},
}

func colors(brand string) (string, string) {
	f := fnv.New32a()
	f.Write([]byte(brand))
	p := palette[int(f.Sum32())%len(palette)]
	return p[0], p[1]
}

// SVG returns a 400x300 illustration for a product category, tinted per product.
func SVG(category, name string) string {
	c, bg := colors(name)
	const ink = "#1d1f24"
	var body string
	switch category {
	case "headphones":
		body = fmt.Sprintf(`<path d="M120 170 V150 a80 80 0 0 1 160 0 V170" fill="none" stroke="%[2]s" stroke-width="14" stroke-linecap="round"/>
<rect x="98" y="160" width="48" height="78" rx="20" fill="%[1]s"/><rect x="254" y="160" width="48" height="78" rx="20" fill="%[1]s"/>
<rect x="112" y="172" width="20" height="54" rx="10" fill="#fff" opacity=".35"/><rect x="268" y="172" width="20" height="54" rx="10" fill="#fff" opacity=".35"/>`, c, ink)
	case "laptops":
		body = fmt.Sprintf(`<rect x="110" y="80" width="180" height="120" rx="10" fill="%[2]s"/><rect x="120" y="90" width="160" height="100" rx="4" fill="%[1]s"/>
<path d="M85 205 H315 L300 225 H100 Z" fill="%[2]s"/><rect x="180" y="208" width="40" height="5" rx="2.5" fill="#fff" opacity=".5"/>
<circle cx="200" cy="140" r="18" fill="#fff" opacity=".35"/>`, c, ink)
	case "smartphones":
		body = fmt.Sprintf(`<rect x="158" y="55" width="84" height="170" rx="16" fill="%[2]s"/><rect x="165" y="70" width="70" height="140" rx="8" fill="%[1]s"/>
<rect x="188" y="61" width="24" height="4" rx="2" fill="#fff" opacity=".5"/><circle cx="200" cy="140" r="14" fill="#fff" opacity=".35"/>`, c, ink)
	case "smartwatches":
		body = fmt.Sprintf(`<rect x="176" y="50" width="48" height="60" rx="10" fill="%[1]s" opacity=".75"/><rect x="176" y="190" width="48" height="60" rx="10" fill="%[1]s" opacity=".75"/>
<rect x="160" y="100" width="80" height="100" rx="22" fill="%[2]s"/><rect x="170" y="110" width="60" height="80" rx="15" fill="%[1]s"/>
<path d="M200 130 V150 L214 158" stroke="#fff" stroke-width="5" stroke-linecap="round" fill="none"/><rect x="240" y="135" width="8" height="22" rx="3" fill="%[2]s"/>`, c, ink)
	case "running shoes":
		body = fmt.Sprintf(`<path d="M95 200 C95 170 120 150 150 140 L185 105 C195 98 210 102 214 114 L232 150 C260 160 300 165 312 182 C318 192 314 205 300 205 H110 C100 205 95 205 95 200 Z" fill="%[1]s"/>
<path d="M92 205 H314 V214 C314 220 309 224 303 224 H102 C96 224 92 220 92 214 Z" fill="%[2]s"/>
<path d="M190 125 L212 135 M180 138 L204 148 M170 151 L196 161" stroke="#fff" stroke-width="5" stroke-linecap="round" opacity=".7"/>`, c, ink)
	case "coffee makers":
		body = fmt.Sprintf(`<rect x="120" y="60" width="130" height="34" rx="8" fill="%[2]s"/><rect x="130" y="94" width="20" height="120" fill="%[2]s"/>
<rect x="120" y="210" width="150" height="16" rx="6" fill="%[2]s"/><rect x="160" y="94" width="90" height="40" rx="6" fill="%[1]s"/>
<path d="M175 160 H235 V195 a12 12 0 0 1 -12 12 H187 a12 12 0 0 1 -12 -12 Z" fill="%[1]s"/><path d="M235 168 h10 a10 10 0 0 1 0 20 h-10" stroke="%[1]s" stroke-width="6" fill="none"/>
<path d="M195 150 q5 -8 0 -16 M215 150 q5 -8 0 -16" stroke="%[1]s" stroke-width="4" fill="none" stroke-linecap="round" opacity=".6"/>`, c, ink)
	case "backpacks":
		body = fmt.Sprintf(`<path d="M175 70 a25 25 0 0 1 50 0" stroke="%[2]s" stroke-width="10" fill="none"/><rect x="140" y="70" width="120" height="160" rx="36" fill="%[1]s"/>
<rect x="160" y="160" width="80" height="52" rx="14" fill="%[2]s" opacity=".85"/><rect x="185" y="172" width="30" height="6" rx="3" fill="#fff" opacity=".6"/>
<rect x="158" y="100" width="84" height="8" rx="4" fill="#fff" opacity=".35"/>`, c, ink)
	case "monitors":
		body = fmt.Sprintf(`<rect x="80" y="60" width="240" height="145" rx="10" fill="%[2]s"/><rect x="90" y="70" width="220" height="125" rx="4" fill="%[1]s"/>
<rect x="190" y="205" width="20" height="25" fill="%[2]s"/><rect x="150" y="228" width="100" height="10" rx="5" fill="%[2]s"/>
<path d="M110 170 L150 130 L180 155 L220 110 L290 170 Z" fill="#fff" opacity=".3"/>`, c, ink)
	default:
		body = fmt.Sprintf(`<rect x="140" y="90" width="120" height="120" rx="20" fill="%s"/>`, c)
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 400 300" role="img"><rect width="400" height="300" fill="%s"/>%s</svg>`, bg, body)
}
