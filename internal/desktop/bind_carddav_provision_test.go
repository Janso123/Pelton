package desktop

import (
	"testing"

	pcarddav "github.com/peltonapp/Pelton/internal/carddav"
	"github.com/peltonapp/Pelton/internal/storage"
)

func TestBooksToAddSkipsExistingURLAndPath(t *testing.T) {
	found := []pcarddav.Book{
		{Path: "/books/a/", Name: "A"},
		{Path: "/books/b/", Name: "B"},
	}
	existing := []storage.AddressBook{
		{URL: "https://dav.example", CollectionPath: "/books/a/"},
	}
	got := booksToAdd(found, "https://dav.example", existing)
	if len(got) != 1 || got[0].Path != "/books/b/" {
		t.Fatalf("got %+v, want only /books/b/", got)
	}
}

func TestBooksToAddKeepsDifferentURL(t *testing.T) {
	found := []pcarddav.Book{{Path: "/books/a/", Name: "A"}}
	existing := []storage.AddressBook{
		{URL: "https://other.example", CollectionPath: "/books/a/"},
	}
	got := booksToAdd(found, "https://dav.example", existing)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
}
