package main

import (
	"fmt"
	"log"

	"github.com/gofiber/fiber/v2"
)

func main() {
	// Fiber instance
	app := fiber.New(fiber.Config{
		BodyLimit: 16 * 1024 * 1024, // this is the default limit of 4MB
	})

	app.Get("/status", func(c *fiber.Ctx) error {
		log.Println("/status called.")
		return c.SendString("Hi ✋")
	})

	app.Post("/upload", func(c *fiber.Ctx) error {
		log.Println("/upload called.")

		// Get first file from form field "document":
		file, err := c.FormFile("file")
		if err != nil {
			return err
		}
		// (uploads_relative) folder must be created before hand:
		// Save file using a relative path:
		return c.SaveFile(file, fmt.Sprintf("/tmp/uploads_relative/%s", file.Filename))
	})

	// Start server
	listen := "localhost:3121"
	log.Println("my_app Listening on: ", listen)
	log.Fatal(app.Listen(listen))
}
