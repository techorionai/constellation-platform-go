// Package valkey is the Valkey adapter of the event streams.
//
// [Publisher] adds the events of one app to the stream events.stream:<app>. It
// implements outbox.Publisher. [Group] is one consumer of a consumer group. It
// implements streams.Group. [Lag] and [Groups] read the state of a stream.
// [TrimPurger] implements retention.Purger and trims the stream of the service.
//
// The functions take the raw valkey-go client (the field Raw of
// clients/valkey.Client). The stream keys do not use the Keyspace of the
// service. The ACL user of a service needs the selectors of [ACLSelectors].
//
//	pub := streamsvalkey.NewPublisher(client.Raw, "helpdesk")
//	group, err := streamsvalkey.NewGroup(client.Raw, streamsvalkey.GroupConfig{
//		App: "helpdesk", Stream: "identity", Group: "helpdesk",
//	})
//
// The ACL cannot isolate the consumer groups of different services. A service
// can acknowledge or claim the messages of the group of another service. All
// services are platform code, and they cannot add events to the stream of
// another app.
package valkey
