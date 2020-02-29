package fuse

// Max pages per fuse message.
const maxPages = 32

// Maximum file write size we are prepared to receive from the kernel.
//
// This number is just a guess.
const maxWrite = maxPages * 4096
