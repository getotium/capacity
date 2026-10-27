package capacity

import (
	"context"
	"errors"
	"fmt"
)

// The fan-out wrappers implement ImagePruner over their children. Listing returns everything the
// children that answered found, joined with the errors of those that did not (see ImagePruner);
// a child that does not implement ImagePruner contributes nothing. Deletes route by Region
// (MultiRegionProvider) or Provider (MultiCloudProvider), like Terminate.

func (m *MultiRegionProvider) ListImages(ctx context.Context) ([]OwnedImage, error) {
	var all []OwnedImage
	var errs []error
	for _, region := range m.Regions() {
		ip, ok := m.providers[region].(ImagePruner)
		if !ok {
			continue
		}
		got, err := ip.ListImages(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list images %s: %w", region, err))
			continue
		}
		for i := range got {
			got[i].Region = region
		}
		all = append(all, got...)
	}
	return all, errors.Join(errs...)
}

func (m *MultiRegionProvider) ListImageSnapshots(ctx context.Context) ([]OwnedSnapshot, error) {
	var all []OwnedSnapshot
	var errs []error
	for _, region := range m.Regions() {
		ip, ok := m.providers[region].(ImagePruner)
		if !ok {
			continue
		}
		got, err := ip.ListImageSnapshots(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list snapshots %s: %w", region, err))
			continue
		}
		for i := range got {
			got[i].Region = region
		}
		all = append(all, got...)
	}
	return all, errors.Join(errs...)
}

func (m *MultiRegionProvider) DeleteImage(ctx context.Context, img OwnedImage) error {
	ip, err := m.imagePrunerFor(img.Region)
	if err != nil {
		return err
	}
	return ip.DeleteImage(ctx, img)
}

func (m *MultiRegionProvider) DeleteSnapshot(ctx context.Context, snap OwnedSnapshot) error {
	ip, err := m.imagePrunerFor(snap.Region)
	if err != nil {
		return err
	}
	return ip.DeleteSnapshot(ctx, snap)
}

func (m *MultiRegionProvider) imagePrunerFor(region string) (ImagePruner, error) {
	p, err := m.providerFor(region)
	if err != nil {
		return nil, err
	}
	ip, ok := p.(ImagePruner)
	if !ok {
		return nil, fmt.Errorf("region %s does not prune images", region)
	}
	return ip, nil
}

func (m *MultiCloudProvider) ListImages(ctx context.Context) ([]OwnedImage, error) {
	var all []OwnedImage
	var errs []error
	for _, name := range m.Providers() {
		ip, ok := m.providers[name].(ImagePruner)
		if !ok {
			continue
		}
		got, err := ip.ListImages(ctx) // a child's partial answer is kept with its error
		if err != nil {
			errs = append(errs, fmt.Errorf("list images %s: %w", name, err))
		}
		for i := range got {
			if got[i].Provider == "" {
				got[i].Provider = name
			}
		}
		all = append(all, got...)
	}
	return all, errors.Join(errs...)
}

func (m *MultiCloudProvider) ListImageSnapshots(ctx context.Context) ([]OwnedSnapshot, error) {
	var all []OwnedSnapshot
	var errs []error
	for _, name := range m.Providers() {
		ip, ok := m.providers[name].(ImagePruner)
		if !ok {
			continue
		}
		got, err := ip.ListImageSnapshots(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list snapshots %s: %w", name, err))
		}
		for i := range got {
			if got[i].Provider == "" {
				got[i].Provider = name
			}
		}
		all = append(all, got...)
	}
	return all, errors.Join(errs...)
}

func (m *MultiCloudProvider) DeleteImage(ctx context.Context, img OwnedImage) error {
	ip, err := m.imagePrunerFor(img.Provider)
	if err != nil {
		return err
	}
	return ip.DeleteImage(ctx, img)
}

func (m *MultiCloudProvider) DeleteSnapshot(ctx context.Context, snap OwnedSnapshot) error {
	ip, err := m.imagePrunerFor(snap.Provider)
	if err != nil {
		return err
	}
	return ip.DeleteSnapshot(ctx, snap)
}

func (m *MultiCloudProvider) imagePrunerFor(name string) (ImagePruner, error) {
	p, err := m.providerFor(name)
	if err != nil {
		return nil, err
	}
	ip, ok := p.(ImagePruner)
	if !ok {
		return nil, fmt.Errorf("provider %s does not prune images", name)
	}
	return ip, nil
}
