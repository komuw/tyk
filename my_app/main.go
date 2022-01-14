package main

import (
	"fmt"
	"github.com/gofiber/fiber/v2"
	"io/ioutil"
	"log"
	"os"
)

func main() {
	// lf := logF()
	// defer lf.Close()
	logger := log.New(os.Stderr, "uploads", log.LstdFlags)
	uploadsDir := "/tmp/uploads_relative"
	makeDirectoryIfNotExists(uploadsDir)

	// Fiber instance
	app := fiber.New(fiber.Config{
		BodyLimit: 16 * 1024 * 1024, // this is the default limit of 4MB
	})

	app.Get("/status", func(c *fiber.Ctx) error {
		logger.Println("/status called.")
		return c.SendString("Hi ✋")
	})

	app.Post("/upload", func(c *fiber.Ctx) error {
		logger.Println("/upload called.")

		// Get first file from form field "document":
		file, err := c.FormFile("file")
		if err != nil {
			return err
		}
		// (uploads_relative) folder must be created before hand:
		// Save file using a relative path:
		return c.SaveFile(file, fmt.Sprintf("%s/%s", uploadsDir, file.Filename))
	})

	// Start server
	listen := "localhost:3121"
	logger.Println("my_app Listening on: ", listen)
	err := app.Listen(listen)
	if err != nil {
		panic(err)
	}
}

func logF() *os.File {
	file, err := ioutil.TempFile("/tmp", "uploads-logs")
	if err != nil {
		panic(err)
	}
	// defer os.Remove(file.Name())

	return file
}

func makeDirectoryIfNotExists(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		e := os.Mkdir(path, os.ModeDir|0755)
		if err != nil {
			fmt.Println("\n\n\t error: ", e)
			panic(e)
		}
	}
	return nil
}
