package graph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/auriora/onemount/pkg/graph/api"
	"github.com/auriora/onemount/pkg/logging"
)

// DriveTypePersonal represents the value for a personal drive's type when fetched from the API.
const (
	DriveTypePersonal = "personal"
	// Other possible values include "business" and "documentLibrary"
)

// Type aliases for backward compatibility during migration
// These will be removed once all code is updated to use api.* types directly
type DriveItem = api.DriveItem
type DriveItemParent = api.DriveItemParent
type Folder = api.Folder
type File = api.File
type Hashes = api.Hashes
type Deleted = api.Deleted

// getItem is the internal method used to lookup items
func getItem(path string, auth *Auth) (*DriveItem, error) {
	body, err := Get(path, auth)
	if err != nil {
		return nil, err
	}
	item := &DriveItem{}
	err = json.Unmarshal(body, item)
	if err != nil && bytes.Contains(body, []byte("\"size\":-")) {
		// onedrive for business directories can sometimes have negative sizes,
		// handle this case by creating a custom unmarshaler
		var rawItem map[string]interface{}
		if jsonErr := json.Unmarshal(body, &rawItem); jsonErr == nil {
			// Set size to 0 for the item
			item.Size = 0
			// Clear the error since we've handled it
			err = nil
		}
	}
	return item, err
}

// GetItem fetches a DriveItem by ID. ID can also be "root" for the root item.
func GetItem(id string, auth *Auth) (*DriveItem, error) {
	return getItem(IDPath(id), auth)
}

// GetItemChild fetches the named child of an item.
func GetItemChild(id string, name string, auth *Auth) (*DriveItem, error) {
	return getItem(
		fmt.Sprintf("%s:/%s", IDPath(id), url.PathEscape(name)),
		auth,
	)
}

// GetItemPath fetches a DriveItem by path. Only used in special cases, like for the
// root item.
func GetItemPath(path string, auth *Auth) (*DriveItem, error) {
	return getItem(ResourcePath(path), auth)
}

// GetItemContent retrieves an item's content from the Graph endpoint.
func GetItemContent(id string, auth *Auth) ([]byte, uint64, error) {
	buf := bytes.NewBuffer(make([]byte, 0))
	n, err := GetItemContentStream(id, auth, buf)
	return buf.Bytes(), n, err
}

// GetItemContentStream is the same as GetItemContent, but writes data to an
// output reader. This function assumes a brand-new io.Writer is used, so
// "output" must be truncated if there is content already in the io.Writer
// prior to use.
func GetItemContentStream(id string, auth *Auth, output io.Writer) (uint64, error) {
	// determine the size of the item
	item, err := GetItem(id, auth)
	if err != nil {
		return 0, err
	}

	const downloadChunkSize = 10 * 1024 * 1024
	downloadURL := fmt.Sprintf("/me/drive/items/%s/content", id)
	if item.Size <= downloadChunkSize {
		// simple one-shot download
		content, err := Get(downloadURL, auth)
		if err != nil {
			return 0, err
		}
		n, err := output.Write(content)
		return uint64(n), err
	}

	// multipart download
	var n uint64
	for i := 0; i < int(item.Size/downloadChunkSize)+1; i++ {
		start := i * downloadChunkSize
		end := start + downloadChunkSize - 1
		logging.Info().
			Str("id", item.ID).
			Str("name", item.Name).
			Msgf("Downloading bytes %d-%d/%d.", start, end, item.Size)
		content, err := Get(downloadURL, auth, Header{
			key:   "Range",
			value: fmt.Sprintf("bytes=%d-%d", start, end),
		})
		if err != nil {
			return n, err
		}
		written, err := output.Write(content)
		n += uint64(written)
		if err != nil {
			return n, err
		}
	}
	logging.Info().
		Str("id", item.ID).
		Str("name", item.Name).
		Uint64("size", n).
		Msgf("Download completed!")
	return n, nil
}

// Remove removes a directory or file by ID
func Remove(id string, auth *Auth) error {
	return Delete("/me/drive/items/"+id, auth)
}

// Mkdir creates a directory on the server at the specified parent ID.
func Mkdir(name string, parentID string, auth *Auth) (*DriveItem, error) {
	// create a new folder on the server
	newFolderPost := DriveItem{
		Name:   name,
		Folder: &Folder{},
	}
	bytePayload, _ := json.Marshal(newFolderPost)
	resp, err := Post(childrenPathID(parentID), auth, bytes.NewReader(bytePayload))
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(resp, &newFolderPost)
	return &newFolderPost, err
}

// Rename moves and/or renames an item on the server. The itemName and parentID
// arguments correspond to the *new* basename or id of the parent.
func Rename(itemID string, itemName string, parentID string, auth *Auth) error {
	// start creating patch content for server
	// mutex does not need to be initialized since it is never used locally
	patchContent := DriveItem{
		ConflictBehavior: "replace", // overwrite existing content at new location
		Name:             itemName,
		Parent: &DriveItemParent{
			ID: parentID,
		},
	}

	// apply patch to server copy - note that we don't actually care about the
	// response content, only if it returns an error
	jsonPatch, _ := json.Marshal(patchContent)

	// First attempt
	_, err := Patch("/me/drive/items/"+itemID, auth, bytes.NewReader(jsonPatch))
	if err != nil {
		// If there's an error, log it and retry with a delay
		logging.Warn().Err(err).
			Str("itemID", itemID).
			Str("newName", itemName).
			Str("newParentID", parentID).
			Msg("Error during rename operation, retrying after delay")

		// Wait a second before retrying
		time.Sleep(time.Second)

		// Create a new reader for the retry since the previous one was consumed
		_, err = Patch("/me/drive/items/"+itemID, auth, bytes.NewReader(jsonPatch))

		// If still failing after retry, log a more detailed error
		if err != nil {
			logging.Error().Err(err).
				Str("itemID", itemID).
				Str("newName", itemName).
				Str("newParentID", parentID).
				Msg("Rename operation failed after retry")
		}
	}

	return err
}

// only used for parsing
type driveChildren = api.DriveChildren

// this is the internal method that actually fetches an item's children
func getItemChildren(pollURL string, auth *Auth) ([]*DriveItem, error) {
	logging.Debug().Str("pollURL", pollURL).Msg("Starting getItemChildren")
	fetched := make([]*DriveItem, 0)
	pageCount := 0

	for pollURL != "" {
		pageCount++
		logging.Debug().Str("pollURL", pollURL).Int("pageCount", pageCount).Msg("Fetching page of children")

		logging.Debug().Str("pollURL", pollURL).Int("pageCount", pageCount).Msg("About to call Get for children page")
		body, err := Get(pollURL, auth)
		logging.Debug().Str("pollURL", pollURL).Int("pageCount", pageCount).Err(err).Msg("Returned from Get for children page")

		if err != nil {
			logging.Error().Str("pollURL", pollURL).Int("pageCount", pageCount).Err(err).Msg("Error fetching children page")
			return fetched, err
		}

		logging.Debug().Str("pollURL", pollURL).Int("pageCount", pageCount).Int("bodySize", len(body)).Msg("Unmarshalling response body")
		var pollResult driveChildren
		err = json.Unmarshal(body, &pollResult)
		if err != nil {
			logging.Error().Str("pollURL", pollURL).Int("pageCount", pageCount).Err(err).Msg("Error unmarshalling children response")
			return fetched, err
		}

		// there can be multiple pages of 200 items each (default).
		// continue to next interation if we have an @odata.nextLink value
		logging.Debug().Str("pollURL", pollURL).Int("pageCount", pageCount).Int("childrenCount", len(pollResult.Children)).Str("nextLink", pollResult.NextLink).Msg("Processing children page")
		fetched = append(fetched, pollResult.Children...)
		pollURL = strings.TrimPrefix(pollResult.NextLink, GraphURL)
	}

	logging.Debug().Int("totalFetched", len(fetched)).Msg("Completed getItemChildren")
	return fetched, nil
}

// GetItemChildren fetches all children of an item denoted by ID.
func GetItemChildren(id string, auth *Auth) ([]*DriveItem, error) {
	return getItemChildren(childrenPathID(id), auth)
}

// GetItemChildrenPath fetches all children of an item denoted by path.
func GetItemChildrenPath(path string, auth *Auth) ([]*DriveItem, error) {
	return getItemChildren(childrenPath(path), auth)
}
