// Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
//
// Fault tests for the write-ahead-logged store: restarts, torn writes, failed
// transactions and concurrent writers.
package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"asksauby/internal/model"
)

func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(func(tx *Txn) error {
		o, _ := tx.Order("O5004")
		o.Status = "cancelled"
		tx.PutOrder(o)
		tx.PutRefund(model.Refund{ID: "RF-5004", OrderID: "O5004", CustomerID: "C1001", Amount: 549})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.wal.Close()
	s2, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if o, _ := s2.Order("O5004"); o.Status != "cancelled" {
		t.Fatalf("committed change lost on restart: %s", o.Status)
	}
	if _, ok := s2.RefundFor("O5004"); !ok {
		t.Fatal("refund lost on restart")
	}
}

func TestTornLastWriteIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "")
	s.Tx(func(tx *Txn) error { o, _ := tx.Order("O5002"); o.Status = "cancelled"; tx.PutOrder(o); return nil })
	s.wal.Close()
	f, _ := os.OpenFile(filepath.Join(dir, "wal.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"kind":"order","order":{"id":"O5003","status":"refun`) // crash mid-write
	f.Close()
	s2, err := Open(dir, "")
	if err != nil {
		t.Fatalf("store must open after a torn write: %v", err)
	}
	if o, _ := s2.Order("O5002"); o.Status != "cancelled" {
		t.Fatal("earlier committed write lost")
	}
	if o, _ := s2.Order("O5003"); o.Status != "shipped" {
		t.Fatalf("torn write applied: %s", o.Status)
	}
}

func TestFailedTransactionChangesNothing(t *testing.T) {
	s, _ := Open("", "")
	err := s.Tx(func(tx *Txn) error {
		o, _ := tx.Order("O5001")
		o.Status = "refunded"
		tx.PutOrder(o)
		tx.PutRefund(model.Refund{ID: "RF-5001", OrderID: "O5001", Amount: 349})
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the error")
	}
	if o, _ := s.Order("O5001"); o.Status != "delivered" {
		t.Fatal("partial transaction applied")
	}
	if _, ok := s.RefundFor("O5001"); ok {
		t.Fatal("refund from a failed transaction")
	}
}

func TestConcurrentWritersAndIDs(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "")
	var wg sync.WaitGroup
	ids := sync.Map{}
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Tx(func(tx *Txn) error {
				id, err := tx.NextOrderID()
				if err != nil {
					return err
				}
				if _, dup := ids.LoadOrStore(id, true); dup {
					t.Errorf("duplicate order id %s", id)
				}
				tx.PutOrder(model.Order{ID: id, CustomerID: "C1003", Status: "placed"})
				return nil
			})
		}()
	}
	wg.Wait()
	s.wal.Close()
	s2, _ := Open(dir, "")
	if n := len(s2.OrdersFor("C1003")); n != 40 {
		t.Fatalf("want 40 orders after restart, got %d", n)
	}
}

func TestConversationKeysAreValidated(t *testing.T) {
	s, _ := Open(t.TempDir(), "")
	for _, k := range [][2]string{{"../etc", "0123456789abcdef"}, {"C1001", "../../x"}, {"C1001", "ZZZZ"}, {"", "0123456789abcdef"}} {
		if err := s.AppendMessage(k[0], k[1], model.Message{Content: "x"}); err == nil {
			t.Errorf("accepted bad key %v", k)
		}
	}
	if err := s.SavePhoto("C1001", "../../evil", []byte("x")); err == nil {
		t.Error("photo path traversal accepted")
	}
	if _, err := s.Photo("C1001", "../wal"); err == nil {
		t.Error("photo read traversal accepted")
	}
}
