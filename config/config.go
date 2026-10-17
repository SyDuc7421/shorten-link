package config

import "github.com/kelseyhightower/envconfig"

type Config struct {
	// HTTP
	Port           int    `envconfig:"PORT" default:"8080"`
	BaseURL        string `envconfig:"BASE_URL" default:"http://localhost:8080"`
	AllowedOrigins string `envconfig:"ALLOWED_ORIGINS" default:""` // comma-separated, empty = deny all CORS

	// NodeID must be unique per server instance (0-1023) or short codes can collide.
	NodeID int64 `envconfig:"NODE_ID" default:"1"`

	// MySQL
	MySQLHost     string `envconfig:"MYSQL_HOST" default:"localhost"`
	MySQLPort     int    `envconfig:"MYSQL_PORT" default:"3306"`
	MySQLUser     string `envconfig:"MYSQL_USER" required:"true"`
	MySQLPassword string `envconfig:"MYSQL_PASSWORD" required:"true"`
	MySQLDB       string `envconfig:"MYSQL_DB" required:"true"`

	// Redis
	RedisAddr     string `envconfig:"REDIS_ADDR" default:"localhost:6379"`
	RedisPassword string `envconfig:"REDIS_PASSWORD"`
	RedisDB       int    `envconfig:"REDIS_DB" default:"0"`

	// RabbitMQ
	RabbitMQURL string `envconfig:"RABBITMQ_URL" required:"true"`

	// JWT
	JWTPrivateKeyPath string `envconfig:"JWT_PRIVATE_KEY_PATH" required:"true"`
	JWTPublicKeyPath  string `envconfig:"JWT_PUBLIC_KEY_PATH" required:"true"`

	// Password reset. The link is PasswordResetURL + "?token=...".
	PasswordResetURL string `envconfig:"PASSWORD_RESET_URL" default:"http://localhost:3000/reset-password"`

	// SMTP. Empty SMTPHost = reset links are logged instead of mailed.
	SMTPHost     string `envconfig:"SMTP_HOST"`
	SMTPPort     int    `envconfig:"SMTP_PORT" default:"587"`
	SMTPUsername string `envconfig:"SMTP_USERNAME"`
	SMTPPassword string `envconfig:"SMTP_PASSWORD"`
	SMTPFrom     string `envconfig:"SMTP_FROM" default:"no-reply@localhost"`
}

func Load() (*Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
