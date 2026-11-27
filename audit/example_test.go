package audit_test

import (
	"context"
	"fmt"

	"github.com/techorionai/constellation-platform-go/audit"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

func ExampleEntryWriter_Write() {
	w, err := audit.New(&mock.AuditInserter{}, audit.Config{App: "helpdesk", ClientID: "helpdesk-service"})
	if err != nil {
		panic(err)
	}
	// In a service, ctx comes from transaction.Transactor.WithinTx.
	err = w.Write(context.Background(), audit.Entry{
		Action: "helpdesk:ticket:write",
		Result: audit.Success,
		Target: &audit.Target{Type: "ticket", ID: "T-1"},
	})
	fmt.Println(err)
	// Output: <nil>
}
