package mailer

import (
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
)

type Mailer interface {
	SendMagicLink(to, link string) error
	Configured() bool
}

func New(host string, port int, username, password, from string) Mailer {
	if host == "" || username == "" || password == "" || from == "" {
		return LogMailer{}
	}
	return SMTPMailer{
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		From:     from,
	}
}

type LogMailer struct{}

func (LogMailer) SendMagicLink(to, link string) error {
	log.Printf("magic login link for %s: %s", to, link)
	return nil
}

func (LogMailer) Configured() bool {
	return false
}

type SMTPMailer struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

func (m SMTPMailer) SendMagicLink(to, link string) error {
	addr := net.JoinHostPort(m.Host, fmt.Sprint(m.Port))
	auth := smtp.PlainAuth("", m.Username, m.Password, m.Host)
	msg := strings.Join([]string{
		"From: " + m.From,
		"To: " + to,
		"Subject: Sign in to Donation admin",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		"Use this link to sign in to Donation admin:",
		link,
		"",
		"This link expires in 15 minutes. If you did not request it, you can ignore this email.",
	}, "\r\n")
	return smtp.SendMail(addr, auth, m.From, []string{to}, []byte(msg))
}

func (m SMTPMailer) Configured() bool {
	return true
}
