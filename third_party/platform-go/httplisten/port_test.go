package httplisten

import "testing"

func TestResolvePort(t *testing.T) {
	t.Setenv("PORT", "")
	t.Setenv("HTTP_PORT", "")

	port, source, err := ResolvePort(3005)
	if err != nil {
		t.Fatal(err)
	}
	if port != 3005 || source != "default" {
		t.Fatalf("default = %d from %q, want 3005/default", port, source)
	}

	t.Setenv("PORT", "8080")
	port, source, err = ResolvePort(3005)
	if err != nil {
		t.Fatal(err)
	}
	if port != 8080 || source != "PORT" {
		t.Fatalf("PORT = %d from %q, want 8080/PORT", port, source)
	}

	t.Setenv("PORT", "")
	t.Setenv("HTTP_PORT", "3005")
	port, source, err = ResolvePort(3005)
	if err != nil {
		t.Fatal(err)
	}
	if port != 3005 || source != "HTTP_PORT" {
		t.Fatalf("HTTP_PORT = %d from %q, want 3005/HTTP_PORT", port, source)
	}

	t.Setenv("PORT", "3000")
	t.Setenv("HTTP_PORT", "3005")
	port, source, err = ResolvePort(3005)
	if err != nil {
		t.Fatal(err)
	}
	if port != 3000 || source != "PORT" {
		t.Fatalf("PORT overrides HTTP_PORT: got %d from %q", port, source)
	}
}
