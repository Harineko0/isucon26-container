//go:build !linux

package isureal

func writeUploadedImage(path string, data []byte) error {
	return writeImageFileDurable(path, data)
}
