package moonshot

import _ "embed"

//go:embed profile.json
var profileJSON []byte

func ProfileJSON() []byte { return append([]byte(nil), profileJSON...) }
