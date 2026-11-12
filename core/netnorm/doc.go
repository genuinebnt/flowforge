// Package netnorm implements the target normalization rules of spec
// section 4.2: hostnames, IP addresses, CIDR prefixes and URLs are brought
// to one canonical form before any scope match or graph de-duplication
// key is computed, so that every component (scope service, tool adapters,
// graph ingest, egress proxy) agrees on what a target is.
//
// It is a layer-0 package: both scope and oam depend on it, and it depends
// only on the standard library and golang.org/x/net.
package netnorm
