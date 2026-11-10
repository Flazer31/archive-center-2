package main

import "archivecenter2/goservice/internal/provider"

func tierProviders() []string {
	var ids []string
	for _, p := range provider.Providers {
		if provider.SupportsServiceTier(p.ID) {
			ids = append(ids, p.ID)
		}
	}
	return ids
}
