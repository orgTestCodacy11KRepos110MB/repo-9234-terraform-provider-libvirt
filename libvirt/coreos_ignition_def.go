package libvirt

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	libvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
)

type defIgnition struct {
	Name     string
	PoolName string
	Content  string
}

// Creates a new cloudinit with the defaults
// the provider uses.
func newIgnitionDef() defIgnition {
	ign := defIgnition{}

	return ign
}

// Create a ISO file based on the contents of the CloudInit instance and
// uploads it to the libVirt pool
// Returns a string holding terraform's internal ID of this resource.
func (ign *defIgnition) CreateAndUpload(client *Client) (string, error) {
	virConn := client.libvirt
	if virConn == nil {
		return "", fmt.Errorf(LibVirtConIsNil)
	}

	pool, err := virConn.StoragePoolLookupByName(ign.PoolName)
	if err != nil {
		return "", fmt.Errorf("can't find storage pool '%s'", ign.PoolName)
	}

	client.poolMutexKV.Lock(ign.PoolName)
	defer client.poolMutexKV.Unlock(ign.PoolName)

	// Refresh the pool of the volume so that libvirt knows it is
	// not longer in use.
	if err := waitForSuccess("Error refreshing pool for volume", func() error {
		return virConn.StoragePoolRefresh(pool, 0)
	}); err != nil {
		return "", err
	}

	volumeDef := newDefVolume()
	volumeDef.Name = ign.Name

	ignFile, err := ign.createFile()
	if err != nil {
		return "", err
	}
	defer func() {
		if err = os.Remove(ignFile); err != nil {
			log.Printf("Error while removing tmp Ignition file: %v", err)
		}
	}()

	img, err := newImage(ignFile)
	if err != nil {
		return "", err
	}

	size, err := img.Size()
	if err != nil {
		return "", err
	}

	volumeDef.Capacity.Unit = "B"
	volumeDef.Capacity.Value = size
	volumeDef.Target.Format.Type = "raw"

	volumeDefXML, err := xml.Marshal(volumeDef)
	if err != nil {
		return "", fmt.Errorf("error serializing libvirt volume: %w", err)
	}

	// create the volume
	volume, err := virConn.StorageVolCreateXML(pool, string(volumeDefXML), 0)
	if err != nil {
		return "", fmt.Errorf("error creating libvirt volume for Ignition %s: %w", ign.Name, err)
	}

	// upload ignition file
	err = img.Import(newCopier(virConn, &volume, volumeDef.Capacity.Value), volumeDef)
	if err != nil {
		return "", fmt.Errorf("error while uploading ignition file %s: %w", img.String(), err)
	}

	if volume.Key == "" {
		return "", fmt.Errorf("error retrieving volume key")
	}

	return ign.buildTerraformKey(volume.Key), nil
}

// create a unique ID for terraform use
// The ID is made by the volume ID (the internal one used by libvirt)
// joined by the ";" with a UUID.
func (ign *defIgnition) buildTerraformKey(volumeKey string) string {
	return fmt.Sprintf("%s;%s", volumeKey, uuid.New())
}

func getIgnitionVolumeKeyFromTerraformID(id string) (string, error) {
	s := strings.SplitN(id, ";", 2)
	if len(s) != 2 {
		return "", fmt.Errorf("%s is not a valid key", id)
	}
	return s[0], nil
}

// Dumps the Ignition object - either generated by Terraform or supplied as a file -
// to a temporary ignition file.
func (ign *defIgnition) createFile() (string, error) {
	log.Print("Creating Ignition temporary file")
	tempFile, err := os.CreateTemp("", ign.Name)
	if err != nil {
		return "", fmt.Errorf("error creating tmp file: %w", err)
	}
	defer tempFile.Close()

	var file bool
	file = true
	if _, err := os.Stat(ign.Content); err != nil {
		var js map[string]interface{}
		if errConf := json.Unmarshal([]byte(ign.Content), &js); errConf != nil {
			return "", fmt.Errorf("coreos_ignition 'content' is neither a file "+
				"nor a valid json object %s", ign.Content)
		}
		file = false
	}

	if !file {
		if _, err := tempFile.WriteString(ign.Content); err != nil {
			return "", fmt.Errorf("cannot write Ignition object to temporary " +
				"ignition file")
		}
	} else if file {
		ignFile, err := os.Open(ign.Content)
		if err != nil {
			return "", fmt.Errorf("error opening supplied Ignition file %s", ign.Content)
		}
		defer ignFile.Close()
		_, err = io.Copy(tempFile, ignFile)
		if err != nil {
			return "", fmt.Errorf("error copying supplied Igition file to temporary file: %s", ign.Content)
		}
	}
	return tempFile.Name(), nil
}

// Creates a new defIgnition object from provided id.
func newIgnitionDefFromRemoteVol(virConn *libvirt.Libvirt, id string) (defIgnition, error) {
	ign := defIgnition{}

	key, err := getIgnitionVolumeKeyFromTerraformID(id)
	if err != nil {
		return ign, err
	}

	volume, err := virConn.StorageVolLookupByKey(key)
	if err != nil {
		return ign, fmt.Errorf("can't retrieve volume %s: %w", key, err)
	}

	ign.Name = volume.Name
	if ign.Name == "" {
		return ign, fmt.Errorf("error retrieving volume name from key: %s", key)
	}

	volPool, err := virConn.StoragePoolLookupByVolume(volume)
	if err != nil {
		return ign, fmt.Errorf("error retrieving pool for volume: %s", volume.Name)
	}

	ign.PoolName = volPool.Name
	if ign.PoolName == "" {
		return ign, fmt.Errorf("error retrieving pool name for volume: %s", volume.Name)
	}

	return ign, nil
}
