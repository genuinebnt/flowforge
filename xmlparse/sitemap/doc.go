// Package sitemap reads sitemaps (sitemaps.org protocol 0.9): <urlset>
// and <sitemapindex> documents, optionally gzip-compressed, and plain-text
// sitemaps. It streams over the xmlparse Decoder, so it inherits its
// XXE-safe, DTD-free reading, and it bounds both the decompressed size
// (decompression stops one byte past the limit, so a zip bomb costs at
// most MaxBytes) and the number of entries.
//
// It lives inside the xmlparse unit because layer-1 packages may not
// import each other; wellknown handles robots.txt and security.txt.
package sitemap
