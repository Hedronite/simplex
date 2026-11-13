//go:build !unix

package outfile

func setUmask(m int) int { return m }
