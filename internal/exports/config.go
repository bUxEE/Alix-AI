package exports

import (
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/config"
)

// SectionName is the configuration section this package owns.
const SectionName = "exports"

// Defaults and ceilings.
const (
	DefaultMaxRows     = 10_000
	DefaultMaxCells    = 200_000
	DefaultMaxBytes    = 8 << 20
	DefaultFileTTL     = time.Hour
	DefaultCSVExcelBOM = true

	maxRowsCap  = 1_000_000
	maxCellsCap = 10_000_000
	maxBytesCap = 64 << 20
	maxTTLCap   = 30 * 24 * time.Hour
)

// Config bounds export generation. Zero fields take defaults.
type Config struct {
	// MaxRows caps rows across every table/sheet of one export.
	MaxRows int `yaml:"max_rows"`
	// MaxCells caps cells across every table/sheet of one export.
	MaxCells int `yaml:"max_cells"`
	// MaxBytes caps the produced file size.
	MaxBytes int `yaml:"max_bytes"`
	// FileTTL is how long the generated FileRef resolves.
	FileTTL time.Duration `yaml:"file_ttl"`
	// CSVExcelBOM prefixes CSV with a UTF-8 BOM so Excel opens it as UTF-8.
	CSVExcelBOM *bool `yaml:"csv_excel_bom"`
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	bom := DefaultCSVExcelBOM
	return Config{
		MaxRows: DefaultMaxRows, MaxCells: DefaultMaxCells, MaxBytes: DefaultMaxBytes,
		FileTTL: DefaultFileTTL, CSVExcelBOM: &bom,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxRows <= 0 {
		c.MaxRows = d.MaxRows
	}
	if c.MaxCells <= 0 {
		c.MaxCells = d.MaxCells
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = d.MaxBytes
	}
	if c.FileTTL <= 0 {
		c.FileTTL = d.FileTTL
	}
	if c.CSVExcelBOM == nil {
		c.CSVExcelBOM = d.CSVExcelBOM
	}
	return c
}

// Validate refuses values above the ceilings.
func (c Config) Validate() error {
	c = c.withDefaults()
	switch {
	case c.MaxRows > maxRowsCap:
		return fmt.Errorf("exports.max_rows above %d", maxRowsCap)
	case c.MaxCells > maxCellsCap:
		return fmt.Errorf("exports.max_cells above %d", maxCellsCap)
	case c.MaxBytes > maxBytesCap:
		return fmt.Errorf("exports.max_bytes above %d", maxBytesCap)
	case c.FileTTL > maxTTLCap:
		return fmt.Errorf("exports.file_ttl above %s", maxTTLCap)
	}
	return nil
}

var registered = DefaultConfig()

func init() { config.Register(SectionName, &registered) }

// Live returns the registered section (after a successful config.Load).
func Live() Config { return registered }

// ConfigSection returns a pointer to the registered section for Options.Config.
func ConfigSection() *Config { return &registered }
