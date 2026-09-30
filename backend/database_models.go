package main

// Bentuk data ini dipakai bersama oleh PostgreSQL, MySQL/MariaDB, dan SQLite.

type tableColumn struct {
	Name               string  `json:"name"`
	Type               string  `json:"type"`
	Nullable           bool    `json:"nullable"`
	Default            *string `json:"default"`
	Key                string  `json:"key"`
	Editable           bool    `json:"editable"`
	DefinitionEditable bool    `json:"definitionEditable,omitempty"`
	Collation          string  `json:"collation,omitempty"`
	Comment            string  `json:"comment,omitempty"`
	Affinity           string  `json:"affinity,omitempty"`
	PrimaryOrder       int     `json:"primaryOrder,omitempty"`
	Hidden             bool    `json:"hidden,omitempty"`
	Generated          string  `json:"generated,omitempty"`
	DefaultSource      string  `json:"defaultSource,omitempty"`
}

// Field khusus engine bersifat opsional agar respons halaman tetap satu bentuk JSON.
type tablePage struct {
	Columns            []tableColumn     `json:"columns"`
	Rows               [][]*string       `json:"rows"`
	Truncated          [][]bool          `json:"truncated"`
	Versions           []string          `json:"versions"`
	KeyValues          [][]string        `json:"keyValues,omitempty"`
	KeyTypes           [][]string        `json:"keyTypes,omitempty"`
	SQLiteStrict       bool              `json:"sqliteStrict,omitempty"`
	SQLiteWithoutRowID bool              `json:"sqliteWithoutRowid,omitempty"`
	SQLiteDDL          string            `json:"sqliteDDL,omitempty"`
	SQLiteVirtual      bool              `json:"sqliteVirtual,omitempty"`
	PrimaryKey         []string          `json:"primaryKey"`
	Editable           bool              `json:"editable"`
	ReadOnlyReason     string            `json:"readOnlyReason,omitempty"`
	Indexes            []tableIndex      `json:"indexes"`
	Constraints        []tableConstraint `json:"constraints"`
	HasMore            bool              `json:"hasMore"`
	CursorPaging       bool              `json:"cursorPaging"`
	NextCursor         string            `json:"nextCursor,omitempty"`
	TotalRows          *int64            `json:"totalRows,omitempty"`
	Duration           int64             `json:"duration"`
}

type tableIndex struct {
	Name       string   `json:"name"`
	Definition string   `json:"definition"`
	Unique     bool     `json:"unique"`
	Primary    bool     `json:"primary"`
	Method     string   `json:"method"`
	Columns    []string `json:"columns"`
	Managed    bool     `json:"managed"`
	Editable   bool     `json:"editable"`
	Partial    bool     `json:"partial,omitempty"`
	Expression bool     `json:"expression,omitempty"`
	Directions []string `json:"directions,omitempty"`
}

type tableConstraint struct {
	Name             string   `json:"name"`
	Type             string   `json:"type"`
	Definition       string   `json:"definition"`
	Validated        bool     `json:"validated"`
	Columns          []string `json:"columns"`
	ReferenceSchema  string   `json:"referenceSchema,omitempty"`
	ReferenceTable   string   `json:"referenceTable,omitempty"`
	ReferenceColumns []string `json:"referenceColumns"`
	Expression       string   `json:"expression,omitempty"`
}

type tableRowKey struct {
	Values  []string `json:"values"`
	Types   []string `json:"types,omitempty"`
	Version string   `json:"version"`
}

type tableMutation struct {
	Database string        `json:"database"`
	Schema   string        `json:"schema"`
	Table    string        `json:"table"`
	Column   string        `json:"column"`
	Value    *string       `json:"value"`
	Row      tableRowKey   `json:"row"`
	Rows     []tableRowKey `json:"rows"`
}

type schemaChange struct {
	Database         string   `json:"database"`
	Schema           string   `json:"schema"`
	Table            string   `json:"table"`
	Name             string   `json:"name"`
	OldName          string   `json:"oldName"`
	Type             string   `json:"type"`
	Columns          []string `json:"columns"`
	Directions       []string `json:"directions,omitempty"`
	Unique           bool     `json:"unique"`
	ReferenceSchema  string   `json:"referenceSchema"`
	ReferenceTable   string   `json:"referenceTable"`
	ReferenceColumns []string `json:"referenceColumns"`
	Expression       string   `json:"expression"`
	ValidateOnly     bool     `json:"validateOnly"`
	PreviewOnly      bool     `json:"previewOnly,omitempty"`
	RebuildName      string   `json:"rebuildName,omitempty"`
	PreviewHash      string   `json:"previewHash,omitempty"`
}

type tableColumnInput struct {
	Name              string  `json:"name"`
	Type              string  `json:"type"`
	Nullable          bool    `json:"nullable"`
	Default           *string `json:"default"`
	Primary           bool    `json:"primary"`
	AutoIncrement     bool    `json:"autoIncrement"`
	Unique            bool    `json:"unique,omitempty"`
	Check             string  `json:"check,omitempty"`
	ReferenceTable    string  `json:"referenceTable,omitempty"`
	ReferenceColumn   string  `json:"referenceColumn,omitempty"`
	DefaultExpression bool    `json:"defaultExpression,omitempty"`
}

type tableChange struct {
	Database string             `json:"database"`
	Schema   string             `json:"schema"`
	Table    string             `json:"table"`
	NewName  string             `json:"newName"`
	Engine   string             `json:"engine,omitempty"`
	Columns  []tableColumnInput `json:"columns"`
}

type tableColumnChange struct {
	Database          string  `json:"database"`
	Schema            string  `json:"schema"`
	Table             string  `json:"table"`
	Name              string  `json:"name"`
	OldName           string  `json:"oldName"`
	Type              string  `json:"type"`
	Nullable          *bool   `json:"nullable"`
	Default           *string `json:"default"`
	ChangeDefault     bool    `json:"changeDefault"`
	DefaultExpression bool    `json:"defaultExpression,omitempty"`
	PreviewOnly       bool    `json:"previewOnly,omitempty"`
	RebuildName       string  `json:"rebuildName,omitempty"`
	PreviewHash       string  `json:"previewHash,omitempty"`
}
