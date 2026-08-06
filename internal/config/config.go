package config

import (
	"bufio"
	"log"
	"os"
	"strings"
)

type Config struct {
	Port             string
	AppEnv           string
	JWTSecret        string
	DBHost           string
	DBPort           string
	DBUser           string
	DBPassword       string
	DBName           string
	DBSSLMode        string
	RedisAddr        string
	RedisPassword    string
	CentrifugoAPIURL string
	CentrifugoSecret string
	CentrifugoAPIKey string
	SMTPHost         string
	SMTPPort         string
	SMTPEmail        string
	SMTPPassword     string
}

var AppConfig *Config

func LoadConfig() {
	loadEnvFile(".env")

	appEnv := getEnv("APP_ENV", "development")

	dbPassword := getEnv("DB_PASSWORD", "")
	if dbPassword == "" {
		if appEnv == "production" {
			log.Fatal("FATAL: DB_PASSWORD is required in production")
		}
		dbPassword = "postgres"
		log.Println("WARN: DB_PASSWORD not set — using insecure default (development only)")
	}

	AppConfig = &Config{
		Port:             getEnv("PORT", "8082"),
		AppEnv:           appEnv,
		JWTSecret:        requireEnv("JWT_SECRET"),
		DBHost:           getEnv("DB_HOST", "localhost"),
		DBPort:           getEnv("DB_PORT", "5432"),
		DBUser:           getEnv("DB_USER", "postgres"),
		DBPassword:       dbPassword,
		DBName:           getEnv("DB_NAME", "quizzzone"),
		DBSSLMode:        getEnv("DB_SSLMODE", defaultSSLMode(appEnv)),
		RedisAddr:        getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:    getEnv("REDIS_PASSWORD", ""),
		CentrifugoAPIURL: getEnv("CENTRIFUGO_API_URL", "http://localhost:8000/api"),
		CentrifugoSecret: requireEnv("CENTRIFUGO_SECRET"),
		CentrifugoAPIKey: requireEnv("CENTRIFUGO_API_KEY"),
		SMTPHost:         getEnv("SMTP_HOST", "smtp.gmail.com"),
		SMTPPort:         getEnv("SMTP_PORT", "587"),
		SMTPEmail:        getEnv("SMTP_EMAIL", ""),
		SMTPPassword:     getEnv("SMTP_PASSWORD", ""),
	}
}

func defaultSSLMode(appEnv string) string {
	if appEnv == "production" {
		return "require"
	}
	return "disable"
}

func getEnv(key, defaultVal string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultVal
}

func requireEnv(key string) string {
	value, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(value) == "" {
		log.Fatalf("FATAL: Required environment variable %q is not set. "+
			"Server cannot start without it. Please set it in your .env file or environment.", key)
	}
	return value
}

func loadEnvFile(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
				val = val[1 : len(val)-1]
			}
			os.Setenv(key, val)
		}
	}
}
