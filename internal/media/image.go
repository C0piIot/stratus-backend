package media

import (
	"errors"
	"fmt"
	"io"

	"github.com/evanoberholster/imagemeta"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// extractImage reads EXIF straight off the blob, seeking rather than
// downloading: the metadata sits at the head of every format here, so a photo
// in a bucket costs a ranged read of a few kilobytes and not the whole file.
func extractImage(r io.ReadSeeker) (db.Media, error) {
	exif, err := imagemeta.Decode(r)
	if err != nil {
		if errors.Is(err, imagemeta.ErrNoExif) {
			// A PNG from a screenshot has no EXIF, and that is not a failure:
			// it is a file we now know nothing more about.
			return db.Media{Kind: db.KindImage}, nil
		}
		return db.Media{}, fmt.Errorf("read exif: %w", err)
	}

	m := db.Media{
		Kind:        db.KindImage,
		TakenAt:     exif.ExifIFD.DateTimeOriginal,
		Width:       int(exif.ExifIFD.PixelXDimension),
		Height:      int(exif.ExifIFD.PixelYDimension),
		Orientation: int(exif.IFD0.Orientation),
		Camera:      camera(exif.IFD0.Make, exif.IFD0.Model),
	}
	if m.TakenAt.IsZero() {
		// Scanners and some phones only write ModifyDate.
		m.TakenAt = exif.IFD0.ModifyDate
	}
	if m.Width == 0 || m.Height == 0 {
		m.Width, m.Height = int(exif.IFD0.ImageWidth), int(exif.IFD0.ImageHeight)
	}

	// Null Island is treated as no position. A photo taken at exactly zero by
	// zero is a rounding artefact far more often than it is the Atlantic.
	if lat, lon := exif.GPS.Latitude(), exif.GPS.Longitude(); lat != 0 || lon != 0 {
		m.GPS = &db.GPS{Latitude: lat, Longitude: lon}
	}
	return m, nil
}

// camera joins make and model without repeating the make, because "Apple" and
// "Apple iPhone 15 Pro" would otherwise become "Apple Apple iPhone 15 Pro".
func camera(make, model string) string {
	switch {
	case make == "":
		return model
	case model == "":
		return make
	case len(model) >= len(make) && model[:len(make)] == make:
		return model
	default:
		return make + " " + model
	}
}
