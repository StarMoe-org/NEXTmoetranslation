//go:build darwin || linux

package lyricsacquisition

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
)

type moderncSQLiteSerializer interface {
	Serialize() ([]byte, error)
	Deserialize([]byte) error
}

func verifyModerncSQLiteMainFile(connection driver.Conn, _ trustedStat) error {
	value := reflect.ValueOf(connection)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("SQLite driver connection has no inspectable concrete identity")
	}
	element := value.Elem()
	connectionType := element.Type()
	if connectionType.PkgPath() != "modernc.org/sqlite" || connectionType.Name() != "conn" {
		return fmt.Errorf("SQLite driver connection type %T is not the reviewed modernc connection", connection)
	}
	serializer, ok := connection.(moderncSQLiteSerializer)
	if !ok {
		return errors.New("reviewed modernc SQLite connection has no stable serialization boundary")
	}
	body, err := serializer.Serialize()
	if err != nil {
		return fmt.Errorf("serialize reviewed modernc SQLite runtime: %w", err)
	}
	if len(body) < 100 || int64(len(body)) > maxMetadataBytes || !bytes.Equal(body[:16], []byte("SQLite format 3\x00")) {
		return errors.New("reviewed modernc SQLite runtime serialization is invalid")
	}
	return nil
}
