// Package all registers every decision-model provider. Import it for its side
// effects: import _ "github.com/talbet/traits/internal/provider/all".
package all

import (
	_ "github.com/talbet/traits/internal/provider/cloudflare"
	_ "github.com/talbet/traits/internal/provider/openai"
	_ "github.com/talbet/traits/internal/provider/opencode"
	_ "github.com/talbet/traits/internal/provider/openrouter"
	_ "github.com/talbet/traits/internal/provider/systemone"
)
