package main

import (
	"bytes"
	"io"
	"log"
	"mime/multipart"
	"net/http"
)

func uploadDotBeer(data []byte, filename string) (string, error) {

	x := new(bytes.Buffer)
	wr := multipart.NewWriter(x)

	formfile, err := wr.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}

	formfile.Write(data)
	wr.WriteField("url_len", "16")
	wr.WriteField("expiry", "86400")

	wr.Close()

	if resp, err := http.Post("https://upload.beer", wr.FormDataContentType(), bytes.NewReader(x.Bytes())); err != nil {
		return "", err
	} else {
		log.Println("fileholed", resp.Status)
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}
