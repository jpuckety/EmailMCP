package smtp

import "testing"

func TestShouldSaveToSent(t *testing.T) {
	if !ShouldSaveToSent("true", "smtp.gmail.com") {
		t.Fatal("explicit true")
	}
	if ShouldSaveToSent("false", "smtp.mail.us-east-1.awsapps.com") {
		t.Fatal("explicit false")
	}
	if ShouldSaveToSent("auto", "smtp.gmail.com") {
		t.Fatal("gmail auto should skip sent copy")
	}
	if !ShouldSaveToSent("auto", "smtp.mail.us-east-1.awsapps.com") {
		t.Fatal("workmail auto should save")
	}
	if !ShouldSaveToSent("auto", "imap.example.com") {
		t.Fatal("unknown auto should save")
	}
}

func TestProviderMessageLimit_WorkMail(t *testing.T) {
	n, name := ProviderMessageLimit("smtp.mail.us-west-2.awsapps.com")
	if n != WorkMailMIMEMax || name != "Amazon WorkMail" {
		t.Fatalf("%d %s", n, name)
	}
	n, name = ProviderMessageLimit("smtp.gmail.com")
	if n != 0 || name != "" {
		t.Fatalf("%d %s", n, name)
	}
}
