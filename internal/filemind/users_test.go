package filemind

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestUserUUIDsPersistWithTransferOwnership(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	addAccount(t, admin, "uuid-another-user", 100)
	created := addAccount(t, admin, "uuid-user", 100)
	response := admin.request("GET", "/admin/api/users", nil, nil)
	checkStatus(t, response, 200)
	var users []User
	if err := json.Unmarshal(response.Body.Bytes(), &users); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, user := range users {
		id, err := uuid.Parse(user.ID)
		if err != nil || id.String() != user.ID || id.Version() != 4 || id.Variant() != uuid.RFC4122 || seen[user.ID] {
			t.Fatalf("invalid or duplicate user UUID: %q", user.ID)
		}
		seen[user.ID] = true
	}
	if len(users) != 3 || !seen[created.ID] {
		t.Fatal("initial or created user is missing")
	}
	owner := newBrowser(a, false)
	loginAs(t, owner, created.Username, accountTestPassword)
	transfer := draft(t, owner, 0, "", "uuid ownership")
	a.Close()
	restarted, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	admin.app, owner.app = restarted, restarted
	stored, err := restarted.store.transfer(context.Background(), transfer.ID)
	if err != nil || stored.UserID != created.ID {
		t.Fatalf("transfer owner changed after restart: owner=%q error=%v", stored.UserID, err)
	}
	response = admin.request("GET", "/admin/api/transfers", nil, nil)
	checkStatus(t, response, 200)
	var transfers []Transfer
	if err := json.Unmarshal(response.Body.Bytes(), &transfers); err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 1 || transfers[0].OwnerID != created.ID || transfers[0].OwnerUsername != created.Username {
		t.Fatalf("admin transfer missing UUID owner: %+v", transfers)
	}
	if readTransfer(t, owner.request("GET", "/api/transfers/"+transfer.ID, nil, nil)).OwnerID != "" {
		t.Fatal("admin owner link leaked into owner response")
	}
}
