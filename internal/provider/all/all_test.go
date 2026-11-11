package all

import (
	"reflect"
	"testing"

	"github.com/talbet/traits/internal/provider"
)

func TestRegistersEveryProvider(t *testing.T) {
	want := []string{"cloudflare", "openai", "opencode", "openrouter", "systemone"}
	if got := provider.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}
