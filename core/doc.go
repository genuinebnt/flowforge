// Package core holds the layer-0 building blocks every other Flowforge
// package depends on: identifiers, the clock and randomness ports, the
// cross-cutting error model, activity classes and canonical encodings
// (spec sections 1.2, 11.1 and 24.4).
//
// core has no dependencies outside the standard library. It is the only
// package allowed to read the system clock or the operating system's
// random source, and it does so only inside SystemClock and SystemRng, the
// production adapters of the Clock and Rng ports. Everything else receives
// time and randomness through those ports so it stays deterministic.
package core
