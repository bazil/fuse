package fuse

// Max pages per fuse message (FUSE_MAX_MAX_PAGES).
const maxPages = 256

// Maximum file write size we are prepared to receive from the kernel.
//
// Linux 4.2.0 has been observed to cap this value at 128kB
// (FUSE_MAX_PAGES_PER_REQ=32, 4kB pages).
//
// Linux >= 4.20 allows up to FUSE_MAX_MAX_PAGES=256, but defaults to
// FUSE_DEFAULT_MAX_PAGES_PER_REQ=32.
const maxWrite = maxPages * 4096
