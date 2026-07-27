package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A trait member carries no visibility modifier of its own — it is
// exactly as reachable as the trait that declares it. Reading the absent
// modifier as "private" made every method of every public trait look
// private, understating the public API surface.
func TestRsExtractor_TraitMembersInheritTraitVisibility(t *testing.T) {
	byID, _ := rustNodesByID(t, `pub trait Handler {
    fn handle(&self);
    fn ready(&self) -> bool { true }
}
trait Internal { fn tick(&self); }
`)
	require.NotNil(t, byID["probe.rs::Handler.handle"])
	assert.Equal(t, VisibilityPublic, byID["probe.rs::Handler.handle"].Meta["visibility"],
		"a public trait's signature is public")
	assert.Equal(t, VisibilityPublic, byID["probe.rs::Handler.ready"].Meta["visibility"],
		"a public trait's default method is public too")
	assert.Equal(t, VisibilityPrivate, byID["probe.rs::Internal.tick"].Meta["visibility"],
		"a private trait's members stay private")
}

// A trait impl's method is callable by anyone who can name the trait,
// whatever the impl block looks like.
func TestRsExtractor_TraitImplMethodsArePublic(t *testing.T) {
	byID, _ := rustNodesByID(t, `struct H;
impl Handler for H { fn handle(&self) {} }
impl H { fn helper(&self) {} }
`)
	assert.Equal(t, VisibilityPublic, byID["probe.rs::H.handle"].Meta["visibility"],
		"reachable through the trait")
	assert.Equal(t, VisibilityPrivate, byID["probe.rs::H.helper"].Meta["visibility"],
		"an inherent impl keeps ordinary per-method visibility")
}

// Trait declarations used to keep the "fn name(...)" placeholder even
// after the free-function and impl-method paths stopped using it.
func TestRsExtractor_TraitMethodSignatureIsReal(t *testing.T) {
	byID, _ := rustNodesByID(t, `pub trait Handler {
    fn ready(&self, tries: u8) -> bool;
}
`)
	assert.Equal(t, "fn ready(&self, tries: u8) -> bool",
		byID["probe.rs::Handler.ready"].Meta["signature"])
}
