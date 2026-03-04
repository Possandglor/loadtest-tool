package main

import (
	"loadtest-tool/database"
	"loadtest-tool/server"
	"log"
)

func main() {
	// Инициализация базы данных
	db, err := database.NewDB("loadtest.db")
	if err != nil {
		log.Fatal("Failed to initialize database:", err)
	}
	defer db.Close()

	// Создание сервера
	srv := server.NewServer(db)
	
	// Настройка маршрутов
	router := srv.SetupRoutes()

	log.Println("Load Test Tool starting on :8080")
	log.Fatal(router.Run(":8080"))
}
