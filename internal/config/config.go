package config

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                 string
	AppEnv               string
	JWTSecret            string
	DBHost               string
	DBPort               string
	DBUser               string
	DBPassword           string
	DBName               string
	DBSSLMode            string
	DBTimeZone           string
	DBMaxOpenConns       int
	DBMaxIdleConns       int
	DBConnMaxLifetimeMin int
	DBConnMaxIdleMin     int
	DBConnectTimeoutSec  int
	DBStatementTimeoutMs int
	RedisAddr            string
	RedisPassword        string
	CentrifugoAPIURL     string
	CentrifugoSecret     string
	CentrifugoAPIKey     string
	SMTPHost             string
	SMTPPort             string
	SMTPEmail            string
	SMTPPassword         string
	// LicenseEnforcement gates the Free/Pro limits in internal/pkg/license. It is
	// config, not a build-time constant, so enabling or disabling commercial gates
	// is a restart — not a rebuild and redeploy of the image.
	LicenseEnforcement bool
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
		Port:       getEnv("PORT", "8082"),
		AppEnv:     appEnv,
		JWTSecret:  requireEnv("JWT_SECRET"),
		DBHost:     getEnv("DB_HOST", "localhost"),
		DBPort:     getEnv("DB_PORT", "5432"),
		DBUser:     getEnv("DB_USER", "postgres"),
		DBPassword: dbPassword,
		DBName:     getEnv("DB_NAME", "quizzzone"),
		DBSSLMode:  getEnv("DB_SSLMODE", defaultSSLMode(appEnv)),
		DBTimeZone: getEnv("DB_TIMEZONE", "Asia/Ho_Chi_Minh"),
		// Pool sizing: MaxOpenConns must stay well under Postgres max_connections
		// divided by the number of API replicas. MaxIdleConns matches it so the
		// pool does not close and redial connections between bursts.
		DBMaxOpenConns:       getEnvInt("DB_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns:       getEnvInt("DB_MAX_IDLE_CONNS", 25),
		DBConnMaxLifetimeMin: getEnvInt("DB_CONN_MAX_LIFETIME_MIN", 30),
		DBConnMaxIdleMin:     getEnvInt("DB_CONN_MAX_IDLE_MIN", 5),
		DBConnectTimeoutSec:  getEnvInt("DB_CONNECT_TIMEOUT_SEC", 5),
		DBStatementTimeoutMs: getEnvInt("DB_STATEMENT_TIMEOUT_MS", 15000),
		RedisAddr:            getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:        getEnv("REDIS_PASSWORD", ""),
		CentrifugoAPIURL:     getEnv("CENTRIFUGO_API_URL", "http://localhost:8000/api"),
		CentrifugoSecret:     requireEnv("CENTRIFUGO_SECRET"),
		CentrifugoAPIKey:     requireEnv("CENTRIFUGO_API_KEY"),
		SMTPHost:             getEnv("SMTP_HOST", "smtp.gmail.com"),
		SMTPPort:             getEnv("SMTP_PORT", "587"),
		SMTPEmail:            getEnv("SMTP_EMAIL", ""),
		SMTPPassword:         getEnv("SMTP_PASSWORD", ""),
		LicenseEnforcement:   getEnvBool("LICENSE_ENFORCEMENT", false),
	}

	validateSecurityConfig(AppConfig)
}

func validateSecurityConfig(cfg *Config) {
	if cfg.AppEnv != "production" {
		return
	}

	validateSecret("JWT_SECRET", cfg.JWTSecret, 32)
	validateSecret("CENTRIFUGO_SECRET", cfg.CentrifugoSecret, 32)
	validateSecret("CENTRIFUGO_API_KEY", cfg.CentrifugoAPIKey, 32)
	validateSecret("DB_PASSWORD", cfg.DBPassword, 16)
	validateSecret("REDIS_PASSWORD", cfg.RedisPassword, 16)

	if (cfg.SMTPEmail == "") != (cfg.SMTPPassword == "") {
		log.Fatal("FATAL: SMTP_EMAIL and SMTP_PASSWORD must either both be set or both be empty")
	}

	adminEmail := strings.TrimSpace(os.Getenv("SEED_ADMIN_EMAIL"))
	adminPassword := os.Getenv("SEED_ADMIN_PASSWORD")
	if (adminEmail == "") != (adminPassword == "") {
		log.Fatal("FATAL: SEED_ADMIN_EMAIL and SEED_ADMIN_PASSWORD must either both be set or both be empty")
	}
	if adminPassword != "" {
		validateSecret("SEED_ADMIN_PASSWORD", adminPassword, 16)
	}
}

func validateSecret(name, value string, minLength int) {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	if len(trimmed) < minLength {
		log.Fatalf("FATAL: %s must contain at least %d characters in production", name, minLength)
	}
	for _, marker := range []string{"change_me", "changeme", "default", "password", "secret_here", "example"} {
		if strings.Contains(lower, marker) {
			log.Fatalf("FATAL: %s contains a forbidden placeholder value", name)
		}
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

func getEnvInt(key string, defaultVal int) int {
	raw, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(raw) == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		log.Printf("WARN: %s=%q is not an integer — using default %d", key, raw, defaultVal)
		return defaultVal
	}
	return n
}

// getEnvBool accepts the strconv.ParseBool set: 1/t/T/TRUE/true/True and the
// false equivalents. An unparseable value falls back to the default rather than
// failing startup — but it is logged, because a typo in LICENSE_ENFORCEMENT
// silently leaves every commercial gate open.
func getEnvBool(key string, defaultVal bool) bool {
	raw, exists := os.LookupEnv(key)
	if !exists || strings.TrimSpace(raw) == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		log.Printf("WARN: %s=%q is not a boolean — using default %t", key, raw, defaultVal)
		return defaultVal
	}
	return b
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
			// A real environment variable always wins over the .env file. Setting
			// unconditionally inverted the normal precedence, letting a stale
			// checked-out .env silently override secrets injected by compose or a
			// secrets manager — including JWT_SECRET and DB_PASSWORD.
			if _, exists := os.LookupEnv(key); exists {
				continue
			}
			os.Setenv(key, val)
		}
	}
}
