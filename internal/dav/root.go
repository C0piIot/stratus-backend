package dav

import (
	"encoding/xml"
	"net/http"
	"strings"
)

// The collection at the origin root (#279).
//
// It exists so that the whole server is one WebDAV namespace: a client that
// mounts the root walks into the user's tree and into the generated
// collections beside it, instead of having to be told three URLs nobody but
// us knows. Composing them is legal WebDAV and is what makes the photo folders
// and the playlists visible to a generic client at all.
//
// It is **synthetic and read-only**: nothing is stored here, the children are
// mounts, and a write of any kind is a 405. That is also what makes the names
// around them safe -- the web UI's own routes sit at this level, and a level
// nothing can be created in cannot collide with them.
//
// And it is what the Windows redirector asks for. It probes OPTIONS at the
// origin before it touches the path it was given, and refuses to connect when
// the answer does not advertise DAV (#281). Here the answer is true rather
// than a special case.

// rootMethods is everything this collection answers. No LOCK: there is nothing
// here to hold, and the class 2 below belongs to the writable child.
const rootMethods = "OPTIONS, PROPFIND"

// Root serves the collection at prefix, whose children are mounts elsewhere on
// this server.
//
// children are the names as they appear from here -- "files", "photos" -- and
// the order is the order they are listed in, because a listing with a stable
// order is one a person can read twice.
func Root(prefix string, children ...string) http.Handler {
	prefix = "/" + strings.Trim(prefix, "/")
	if prefix != "/" {
		prefix += "/"
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			// Class 2 at the root although nothing here locks: it is the
			// server that is being asked about, and the tree underneath does
			// lock. Finder reads this once and believes it for everything
			// below, which is the honest answer rather than a convenient one.
			w.Header().Set("DAV", "1, 2")
			w.Header().Set("MS-Author-Via", "DAV")
			w.Header().Set("Allow", rootMethods)
			w.WriteHeader(http.StatusOK)
			return
		case "PROPFIND":
		default:
			w.Header().Set("Allow", rootMethods)
			http.Error(w, "the root of this server is a listing and nothing else",
				http.StatusMethodNotAllowed)
			return
		}
		if refuseInfiniteDepth(w, r) {
			return
		}

		// Hand-written and not x/net's, unlike the other two read-only mounts:
		// those have a tree to walk and this has three names. A FileSystem for
		// it would be more code than the XML it would produce.
		responses := []collectionResponse{{Href: prefix, Name: strings.Trim(prefix, "/")}}
		if r.Header.Get("Depth") == "1" {
			for _, name := range children {
				responses = append(responses, collectionResponse{
					Href: prefix + name + "/",
					Name: name,
				})
			}
		}
		writeMultiStatus(w, responses)
	})
}

// collectionResponse is one collection in a multistatus. Every property here
// is the same for all of them, which is why this is a struct and not a
// property list: a collection that exists, is called something, and holds
// nothing of its own.
type collectionResponse struct {
	Href string
	Name string
}

// writeMultiStatus answers a PROPFIND with the collections given.
func writeMultiStatus(w http.ResponseWriter, responses []collectionResponse) {
	type propstat struct {
		XMLName xml.Name `xml:"D:propstat"`
		Prop    struct {
			DisplayName  string `xml:"D:displayname"`
			ResourceType struct {
				Collection struct{} `xml:"D:collection"`
			} `xml:"D:resourcetype"`
		} `xml:"D:prop"`
		Status string `xml:"D:status"`
	}
	type response struct {
		XMLName  xml.Name `xml:"D:response"`
		Href     string   `xml:"D:href"`
		Propstat propstat
	}
	type multistatus struct {
		XMLName  xml.Name `xml:"D:multistatus"`
		Xmlns    string   `xml:"xmlns:D,attr"`
		Response []response
	}

	out := multistatus{Xmlns: "DAV:"}
	for _, r := range responses {
		var item response
		item.Href = r.Href
		item.Propstat.Prop.DisplayName = r.Name
		item.Propstat.Status = "HTTP/1.1 200 OK"
		out.Response = append(out.Response, item)
	}

	body, err := xml.Marshal(out)
	if err != nil {
		// Nothing here can fail to marshal: the values are three names this
		// server chose. A 500 rather than a half-written body all the same.
		http.Error(w, "building the listing", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}
