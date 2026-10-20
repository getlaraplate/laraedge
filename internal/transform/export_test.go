package transform

// NewChain builds a Chain from ready steps, bypassing the catalogue, so tests
// can drive the chain with fake steps.
var NewChain = newChain
