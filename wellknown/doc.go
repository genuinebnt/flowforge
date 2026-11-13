// Package wellknown parses two well-known text files: robots.txt (RFC
// 9309, Robots Exclusion Protocol) with an Allowed(userAgent, path)
// matcher, and security.txt (RFC 9116) with validation against a caller
// supplied time. Both are line-oriented state machines that never fail as
// a whole: malformed lines become diagnostics with stable codes, and
// limits (robots.txt's 500 KiB of RFC 9309 §2.5, 8 MiB for security.txt,
// 20,000 items each) make the result GradePartial.
//
// Sitemaps are read by xmlparse/sitemap: layer-1 packages may not import
// each other, so the XML-based format lives with the XML parser.
package wellknown
