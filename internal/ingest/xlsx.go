package ingest

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// readXLSX returns the cached cell values of one sheet as a grid. It reads
// only the parts it needs (workbook, rels, shared strings, one sheet),
// never evaluates formulas, and enforces lim on every inflated byte, so a zip
// bomb or a sheet with millions of rows fails fast. An empty sheet name means
// the first sheet.
func readXLSX(data []byte, sheet string, lim Limits) ([][]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: not a valid xlsx: %w", ErrUnsupportedMedia, err)
	}
	budget := &inflateBudget{left: lim.MaxUncompressedSize}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	open := func(name string) ([]byte, error) {
		f, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("%w: xlsx part %s missing", ErrBadMapping, name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: xlsx part %s: %w", ErrUnsupportedMedia, name, err)
		}
		defer func() { _ = rc.Close() }()
		return budget.readAll(rc)
	}

	target, err := sheetTarget(open, sheet, lim)
	if err != nil {
		return nil, err
	}
	var shared []string
	if _, ok := files["xl/sharedStrings.xml"]; ok {
		b, err := open("xl/sharedStrings.xml")
		if err != nil {
			return nil, err
		}
		if shared, err = parseShared(b, lim); err != nil {
			return nil, err
		}
	}
	b, err := open(target)
	if err != nil {
		return nil, err
	}
	return parseSheet(b, shared, lim)
}

type inflateBudget struct{ left int64 }

func (b *inflateBudget) readAll(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, b.left+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedMedia, err)
	}
	if int64(len(data)) > b.left {
		return nil, fmt.Errorf("%w: xlsx inflates past %d bytes", ErrLimitExceeded, b.left)
	}
	b.left -= int64(len(data))
	return data, nil
}

func sheetTarget(open func(string) ([]byte, error), sheet string, lim Limits) (string, error) {
	wb, err := open("xl/workbook.xml")
	if err != nil {
		return "", err
	}
	var w struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wb, &w); err != nil {
		return "", fmt.Errorf("%w: workbook.xml: %w", ErrUnsupportedMedia, err)
	}
	if len(w.Sheets) == 0 {
		return "", fmt.Errorf("%w: workbook has no sheets", ErrBadMapping)
	}
	if lim.MaxSheets > 0 && len(w.Sheets) > lim.MaxSheets {
		return "", fmt.Errorf("%w: %d sheets", ErrLimitExceeded, len(w.Sheets))
	}
	rid := w.Sheets[0].RID
	if sheet != "" {
		rid = ""
		for _, s := range w.Sheets {
			if s.Name == sheet {
				rid = s.RID
			}
		}
		if rid == "" {
			return "", fmt.Errorf("%w: no sheet named %q", ErrBadMapping, sheet)
		}
	}
	rels, err := open("xl/_rels/workbook.xml.rels")
	if err != nil {
		return "", err
	}
	var r struct {
		Rels []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(rels, &r); err != nil {
		return "", fmt.Errorf("%w: workbook rels: %w", ErrUnsupportedMedia, err)
	}
	for _, rel := range r.Rels {
		if rel.ID != rid {
			continue
		}
		t := rel.Target
		if strings.HasPrefix(t, "/") {
			t = strings.TrimPrefix(t, "/")
		} else {
			t = path.Join("xl", t)
		}
		if !strings.HasPrefix(t, "xl/worksheets/") {
			return "", fmt.Errorf("%w: sheet target %q", ErrUnsupportedMedia, rel.Target)
		}
		return t, nil
	}
	return "", fmt.Errorf("%w: sheet relationship %s missing", ErrUnsupportedMedia, rid)
}

func parseShared(b []byte, lim Limits) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	var out []string
	var cur strings.Builder
	inSI, inT := false, false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: sharedStrings: %w", ErrUnsupportedMedia, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = inSI
			case "rPh": // phonetic hints are not cell text
				if err := dec.Skip(); err != nil {
					return nil, fmt.Errorf("%w: sharedStrings: %w", ErrUnsupportedMedia, err)
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				inSI = false
				out = append(out, cur.String())
			case "t":
				inT = false
			}
		case xml.CharData:
			if inT {
				if lim.MaxCellBytes > 0 && cur.Len()+len(t) > lim.MaxCellBytes {
					return nil, fmt.Errorf("%w: shared string over %d bytes", ErrLimitExceeded, lim.MaxCellBytes)
				}
				cur.Write(t)
			}
		}
	}
}

type xlsxCell struct {
	Ref    string `xml:"r,attr"`
	Type   string `xml:"t,attr"`
	V      string `xml:"v"`
	Inline string `xml:"is>t"`
}

func parseSheet(b []byte, shared []string, lim Limits) ([][]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	var grid [][]string
	row := -1
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return grid, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: sheet: %w", ErrUnsupportedMedia, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "row":
			row = len(grid)
			for _, a := range se.Attr {
				if a.Name.Local == "r" {
					if n, err := strconv.Atoi(a.Value); err == nil && n >= 1 {
						row = n - 1
					}
				}
			}
			if lim.MaxRows > 0 && row >= lim.MaxRows {
				return nil, fmt.Errorf("%w: more than %d rows", ErrLimitExceeded, lim.MaxRows)
			}
			for len(grid) <= row {
				grid = append(grid, nil)
			}
		case "c":
			var c xlsxCell
			if err := dec.DecodeElement(&c, &se); err != nil {
				return nil, fmt.Errorf("%w: cell: %w", ErrUnsupportedMedia, err)
			}
			if row < 0 {
				continue
			}
			col := len(grid[row])
			if c.Ref != "" {
				if col, err = colIndex(c.Ref); err != nil {
					return nil, err
				}
			}
			if lim.MaxColumns > 0 && col >= lim.MaxColumns {
				return nil, fmt.Errorf("%w: more than %d columns", ErrLimitExceeded, lim.MaxColumns)
			}
			v := c.V
			switch c.Type {
			case "s":
				i, err := strconv.Atoi(c.V)
				if err != nil || i < 0 || i >= len(shared) {
					return nil, fmt.Errorf("%w: bad shared string index in %s", ErrUnsupportedMedia, c.Ref)
				}
				v = shared[i]
			case "inlineStr":
				v = c.Inline
			}
			if lim.MaxCellBytes > 0 && len(v) > lim.MaxCellBytes {
				return nil, fmt.Errorf("%w: cell %s over %d bytes", ErrLimitExceeded, c.Ref, lim.MaxCellBytes)
			}
			for len(grid[row]) <= col {
				grid[row] = append(grid[row], "")
			}
			grid[row][col] = v
		}
	}
}

// colIndex converts the letters of an A1 reference to a 0-based column.
func colIndex(ref string) (int, error) {
	n := 0
	i := 0
	for ; i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z'; i++ {
		n = n*26 + int(ref[i]-'A'+1)
		if n > 16384 {
			return 0, fmt.Errorf("%w: bad cell reference %q", ErrUnsupportedMedia, ref)
		}
	}
	if i == 0 {
		return 0, fmt.Errorf("%w: bad cell reference %q", ErrUnsupportedMedia, ref)
	}
	return n - 1, nil
}
