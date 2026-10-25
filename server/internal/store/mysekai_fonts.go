package store

import (
	"context"
	"fmt"
	"moesekai/server/internal/textfonts"
)

type MysekaiFontProvider interface {
	Load(context.Context) (textfonts.Catalog, error)
}

func (s *Store) SetMysekaiFontProvider(provider MysekaiFontProvider) { s.mysekaiFonts = provider }
func (s *Store) MysekaiFontCoverage(ctx context.Context) (textfonts.Catalog, error) {
	if s.mysekaiFonts == nil {
		return textfonts.Catalog{}, fmt.Errorf("MYSEKAI 字体目录尚未配置；草稿可保存，发布暂不可用")
	}
	c, e := s.mysekaiFonts.Load(ctx)
	if e != nil {
		return c, fmt.Errorf("MYSEKAI 字体目录暂不可用，草稿已保留：%w", e)
	}
	return c, nil
}
