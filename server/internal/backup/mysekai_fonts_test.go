package backup

import (
	"context"
	"moesekai/server/internal/textfonts"
)

type testMysekaiFonts struct{}

func (testMysekaiFonts) Load(context.Context) (textfonts.Catalog, error) {
	return textfonts.Catalog{Fonts: []textfonts.Font{{Coverage: textfonts.Coverage{{32, 126}, {0x4e00, 0x9fff}}}}}, nil
}
