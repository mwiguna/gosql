package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Replace only pg_dump's outer guards. SQL bodies and COPY data stay byte-for-byte
// intact; a fresh restriction key prevents uploaded scripts using local commands.
func preparePostgresSQL(source *os.File) (_ *os.File, err error) {
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(source)
	var offset, headerStart, headerEnd, footerStart, footerEnd int64
	leading, guard := true, ""
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		start := offset
		offset += int64(len(line))
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		comment := trimmed == "" || strings.HasPrefix(trimmed, "--")
		if leading && !comment {
			leading = false
			fields := strings.Fields(trimmed)
			if len(fields) == 2 && fields[0] == `\restrict` {
				guard = fields[1]
				headerStart, headerEnd = start, offset
			}
		}
		if guard != "" && trimmed == `\unrestrict `+guard {
			footerStart, footerEnd = start, offset
		} else if !comment {
			footerStart, footerEnd = 0, 0
		}
		if readErr == io.EOF {
			break
		}
	}
	prepared, err := os.CreateTemp("", "gosql-sql-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			prepared.Close()
			os.Remove(prepared.Name())
		}
	}()
	if _, err = fmt.Fprintln(prepared, `\restrict `+newID()); err != nil {
		return nil, err
	}
	// Skip a UTF-8 BOM before the first SQL token.
	var bom [3]byte
	if _, err = source.ReadAt(bom[:], 0); err != nil && err != io.EOF {
		return nil, err
	}
	cursor := int64(0)
	if string(bom[:]) == "\ufeff" {
		cursor = 3
	}
	for _, span := range [][2]int64{{headerStart, headerEnd}, {footerStart, footerEnd}, {offset, offset}} {
		if span[1] == 0 {
			continue
		}
		if _, err = source.Seek(cursor, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err = io.CopyN(prepared, source, max(0, span[0]-cursor)); err != nil {
			return nil, err
		}
		cursor = span[1]
	}
	if _, err = prepared.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return prepared, nil
}
