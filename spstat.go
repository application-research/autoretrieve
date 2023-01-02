package main

import (
	ep "github.com/filecoin-project/lassie/pkg/eventpublisher"
)

type sp string
type retrievalStats struct {
	success int
	failure int
}

type SPStatTracker struct {
	stats map[sp]retrievalStats
}

func NewSPStatTracker() *SPStatTracker {
	return &SPStatTracker{
		stats: make(map[sp]retrievalStats),
	}
}

// type: lassie retriever.RetrievalSubscriber
func (*SPStatTracker) OnRetrievalEvent(re ep.RetrievalEvent) {
	if re.Code() == ep.FailureCode {

		// store re.StorageProviderId() as success
		// Handle failure event
	}
	if re.Code() == ep.SuccessCode {
		// handle success event
		// store re.StorageProviderId() as failure
	}
}
