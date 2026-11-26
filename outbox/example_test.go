package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/outbox"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

// ExampleEventWriter_Write uses a mock inserter. A service uses
// outbox/postgres.NewWriter and calls Write inside Transactor.WithinTx.
func ExampleEventWriter_Write() {
	ins := &mock.OutboxInserter{}
	w, err := outbox.New(outbox.Config{App: "helpdesk", Types: []string{"helpdesk.ticket.created"}}, ins)
	if err != nil {
		fmt.Println(err)
		return
	}
	// A request handler uses the principal of the request.
	actor := outbox.ActorFromPrincipal(auth.Principal{ID: "user-1", Type: auth.PrincipalUser})
	err = w.Write(context.Background(), outbox.Event{
		Type:    "helpdesk.ticket.created",
		Subject: "T-1",
		Data:    map[string]any{"title": "Printer"},
		Actor:   actor,
	})
	fmt.Println(err)

	var ce struct {
		Source, Type, Actor, ActorType string
	}
	_ = json.Unmarshal(ins.Rows()[0].Payload, &ce)
	fmt.Println(ce.Source, ce.Type, ce.Actor)
	// Output:
	// <nil>
	// /helpdesk helpdesk.ticket.created user-1
}

// ExampleServiceActor shows the actor of an event with no request principal,
// for example an event of a timer.
func ExampleServiceActor() {
	a := outbox.ServiceActor("helpdesk-service")
	fmt.Println(a.ID, a.Type)

	w, _ := outbox.New(outbox.Config{App: "helpdesk", Types: []string{"helpdesk.ticket.created"}}, &mock.OutboxInserter{NoTx: true})
	err := w.Write(context.Background(), outbox.Event{Type: "helpdesk.ticket.created", Data: map[string]any{}, Actor: a})
	fmt.Println(errors.Is(err, outbox.ErrNoTransaction))
	// Output:
	// helpdesk-service service
	// true
}
