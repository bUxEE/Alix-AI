package apppkg

// Limits bound what the loader reads from a package it does not trust. The
// loader holds a whole package in memory (an immutable snapshot), so every
// bound is also a memory bound. A package or archive that exceeds one is
// refused with a critical finding before anything is parsed or installed.
type Limits struct {
	// MaxArchiveBytes bounds the compressed .tar.gz read from a source.
	MaxArchiveBytes int64
	// MaxTotalBytes bounds the sum of all file sizes in the package.
	MaxTotalBytes int64
	// MaxFileBytes bounds one file.
	MaxFileBytes int64
	// MaxFiles bounds the number of files.
	MaxFiles int
	// MaxDepth bounds the number of path segments of one file path.
	MaxDepth int
	// MaxRatio bounds uncompressed bytes per compressed byte of an archive,
	// checked once an archive has produced RatioFloor bytes, so that a
	// decompression bomb is refused long before MaxTotalBytes is reached.
	MaxRatio int64
	// MaxYAMLBytes bounds one structured (YAML) file.
	MaxYAMLBytes int64
	// MaxYAMLNodes bounds the number of nodes in one YAML document.
	MaxYAMLNodes int
	// MaxYAMLDepth bounds the nesting of one YAML document.
	MaxYAMLDepth int
	// MaxFindings bounds the findings one load reports; the report says so
	// when it is cut.
	MaxFindings int
}

// RatioFloor is the uncompressed size below which MaxRatio is not applied: a
// tiny archive of repetitive text legitimately compresses very well.
const RatioFloor = 1 << 20

// DefaultLimits returns the limits the loader applies unless Options.Limits
// overrides them: generous for a documentation-bearing package (a few
// hundred files, tens of megabytes) and far below what could exhaust the 4 GB
// profile.
func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes: 128 << 20,
		MaxTotalBytes:   256 << 20,
		MaxFileBytes:    64 << 20,
		MaxFiles:        10000,
		MaxDepth:        16,
		MaxRatio:        100,
		MaxYAMLBytes:    2 << 20,
		MaxYAMLNodes:    200000,
		MaxYAMLDepth:    32,
		MaxFindings:     1000,
	}
}

// withDefaults fills every zero field from DefaultLimits, so a partial
// override never leaves a bound at "unlimited".
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxArchiveBytes <= 0 {
		l.MaxArchiveBytes = d.MaxArchiveBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxRatio <= 0 {
		l.MaxRatio = d.MaxRatio
	}
	if l.MaxYAMLBytes <= 0 {
		l.MaxYAMLBytes = d.MaxYAMLBytes
	}
	if l.MaxYAMLNodes <= 0 {
		l.MaxYAMLNodes = d.MaxYAMLNodes
	}
	if l.MaxYAMLDepth <= 0 {
		l.MaxYAMLDepth = d.MaxYAMLDepth
	}
	if l.MaxFindings <= 0 {
		l.MaxFindings = d.MaxFindings
	}
	return l
}
