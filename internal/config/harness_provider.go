package config

import "sort"

// HarnessProviderBlock is a neutral descriptor for one singular [provider.<id>]
// block (IS-GC-RUNTIME-PROVIDER-CONTRACT, Open Question 3: the registry lives in
// city configuration). It carries only the fields the harness registry builder
// needs, so the registry builder does not depend on the full ProviderSpec.
type HarnessProviderBlock struct {
	ID             string
	URL            string
	Token          string
	HarnessSlots   []string
	CapabilityKeys []string
}

// HarnessProviderBlocks returns the city's singular [provider.*] blocks as
// neutral descriptors, sorted by provider id for deterministic registry
// preference order and reproducible replay (AC-REG2). It returns an empty slice
// when no [provider.*] blocks are declared.
func (c *City) HarnessProviderBlocks() []HarnessProviderBlock {
	if c == nil || len(c.Provider) == 0 {
		return nil
	}
	ids := make([]string, 0, len(c.Provider))
	for id := range c.Provider {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	blocks := make([]HarnessProviderBlock, 0, len(ids))
	for _, id := range ids {
		spec := c.Provider[id]
		blocks = append(blocks, HarnessProviderBlock{
			ID:             id,
			URL:            spec.URL,
			HarnessSlots:   append([]string(nil), spec.HarnessSlots...),
			CapabilityKeys: append([]string(nil), spec.CapabilityKeys...),
		})
	}
	return blocks
}
