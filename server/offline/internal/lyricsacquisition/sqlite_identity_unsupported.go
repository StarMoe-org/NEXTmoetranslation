//go:build !darwin && !linux

package lyricsacquisition

import (
	"database/sql/driver"
	"errors"
)

func verifyModerncSQLiteMainFile(driver.Conn, trustedStat) error {
	return errors.New("HOLD: reviewed modernc SQLite descriptor binding is unsupported on this platform")
}
