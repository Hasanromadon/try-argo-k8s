package main

import (
	"log"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

type Config struct {
	Server struct {
		GRPCPort int `mapstructure:"grpc_port"`
		HTTPPort int `mapstructure:"http_port"`
	} `mapstructure:"server"`
	Database struct {
		URL string `mapstructure:"url"`
	} `mapstructure:"database"`
	Vendor struct {
		BillerURL    string `mapstructure:"biller_url"`
		AuthURL      string `mapstructure:"auth_url"`
		ClientID     string `mapstructure:"client_id"`
		ClientSecret string `mapstructure:"client_secret"`
	} `mapstructure:"vendor"`
}

var AppConfig Config

func InitConfig() {
	viper.SetConfigName("application")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./config") // Local dev
	viper.AddConfigPath("/app/config") // Kubernetes ConfigMap

	err := viper.ReadInConfig()
	if err != nil {
		log.Fatalf("Fatal error config file: %v \n", err)
	}

	err = viper.Unmarshal(&AppConfig)
	if err != nil {
		log.Fatalf("Unable to decode into struct, %v", err)
	}

	log.Println("Config loaded successfully.")

	// Zero-Downtime Watcher
	viper.WatchConfig()
	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Println("Config file changed:", e.Name)
		err := viper.Unmarshal(&AppConfig)
		if err != nil {
			log.Println("Error updating config:", err)
		} else {
			log.Println("Config updated successfully in memory.")
		}
	})
}
