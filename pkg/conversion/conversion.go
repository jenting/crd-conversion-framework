package conversion

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sirupsen/logrus"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

type Handler struct {
	scheme  *runtime.Scheme
	decoder *Decoder
}

func NewHandler() (*Handler, error) {
	scheme := runtime.NewScheme()
	// TODO(user): registry the scheme for the types you want to convert below
	// For example:
	// if err := apiextv1.AddToScheme(scheme); err != nil {
	//     return nil, err
	// }
	// if err := apiextv1beta1.AddToScheme(scheme); err != nil {
	//     return nil, err
	// }

	return &Handler{
		scheme:  scheme,
		decoder: NewDecoder(scheme),
	}, nil
}

func (h *Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	var body []byte
	if req.Body != nil {
		if data, err := io.ReadAll(req.Body); err == nil {
			body = data
		}
	}
	convertReview := apiextv1.ConversionReview{}

	err := h.decoder.DecodeInto(body, &convertReview)
	if err != nil {
		logrus.WithError(err).Error("error decoding conversion request")
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}

	convertReview.Response = h.handleConvertRequest(convertReview.Request)
	convertReview.Response.UID = convertReview.Request.UID

	err = json.NewEncoder(rw).Encode(&convertReview)
	if err != nil {
		logrus.WithError(err).Error("error encoding conversion request")
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
}

// handleConvertRequest handles a version conversion request.
func (h *Handler) handleConvertRequest(req *apiextv1.ConversionRequest) *apiextv1.ConversionResponse {
	var convertedObjects []runtime.RawExtension
	for _, obj := range req.Objects {
		src, gvk, err := h.decoder.Decode(obj.Raw)
		if err != nil {
			logrus.WithError(err).Error("error decoding src object")
		}
		logrus.Debugf("decoding incoming obj: src %v gvk %v src type %v", src, gvk, fmt.Sprintf("%T", src))

		dst, err := getTargetObject(h.scheme, req.DesiredAPIVersion, gvk.Kind)
		if err != nil {
			logrus.WithError(err).Error("error getting destination object")
			return conversionResponseFailureWithMessagef("error converting object")
		}
		err = h.convertObject(src, dst)
		if err != nil {
			logrus.WithError(err).Error("error converting object")
			return conversionResponseFailureWithMessagef("error converting object")
		}
		convertedObjects = append(convertedObjects, runtime.RawExtension{Object: dst})
	}
	return &apiextv1.ConversionResponse{
		ConvertedObjects: convertedObjects,
		Result:           statusSucceed(),
	}
}

func (h *Handler) convertObject(src, dst runtime.Object) error {
	if src.GetObjectKind().GroupVersionKind().String() == dst.GetObjectKind().GroupVersionKind().String() {
		return fmt.Errorf("conversion is not allowed between same type %T", src)
	}

	srcIsHub, dstIsHub := isHub(src), isHub(dst)
	srcIsConvertible, dstIsConvertible := isConvertible(src), isConvertible(dst)

	if srcIsHub {
		if dstIsConvertible {
			return dst.(conversion.Convertible).ConvertFrom(src.(conversion.Hub))
		}
		// this is error case.
		return fmt.Errorf("%T is not convertible to", src)
	}

	if dstIsHub {
		if srcIsConvertible {
			return src.(conversion.Convertible).ConvertTo(dst.(conversion.Hub))
		}
		// this is error case.
		return fmt.Errorf("%T is not convertible", src)
	}

	// neither src nor dst are Hub, means both of them are spoke, so lets get the hub
	// version type.
	hub, err := getHub(h.scheme, src)
	if err != nil {
		return err
	}

	// src and dst needs to be convertible for it to work
	if !srcIsConvertible || !dstIsConvertible {
		return fmt.Errorf("%T and %T needs to be both convertible", src, dst)
	}

	err = src.(conversion.Convertible).ConvertTo(hub)
	if err != nil {
		return fmt.Errorf("%T failed to convert to hub version %T : %v", src, hub, err)
	}

	err = dst.(conversion.Convertible).ConvertFrom(hub)
	if err != nil {
		return fmt.Errorf("%T failed to convert from hub version %T : %v", dst, hub, err)
	}

	return nil
}

func getHub(scheme *runtime.Scheme, obj runtime.Object) (conversion.Hub, error) {
	gvks, _, err := scheme.ObjectKinds(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve object kinds for given object : %v", err)
	}

	var hub conversion.Hub
	hubFoundAlready := false
	var isHub bool
	for _, gvk := range gvks {
		o, _ := scheme.New(gvk)
		if hub, isHub = o.(conversion.Hub); isHub {
			if hubFoundAlready {
				// multiple hub found, error case
				return nil, fmt.Errorf("multiple hub version defined")
			}
			hubFoundAlready = true
		}
	}
	return hub, nil
}

func getTargetObject(scheme *runtime.Scheme, apiVersion, kind string) (runtime.Object, error) {
	gvk := schema.FromAPIVersionAndKind(apiVersion, kind)
	obj, err := scheme.New(gvk)
	if err != nil {
		return obj, err
	}

	t, err := meta.TypeAccessor(obj)
	if err != nil {
		return obj, err
	}

	t.SetAPIVersion(apiVersion)
	t.SetKind(kind)
	return obj, nil
}

// conversionResponseFailureWithMessagef is a helper function to create an ConversionResponse
// with a formatted embedded error message
func conversionResponseFailureWithMessagef(msg string, params ...interface{}) *apiextv1.ConversionResponse {
	return &apiextv1.ConversionResponse{
		Result: metav1.Status{
			Message: fmt.Sprintf(msg, params...),
			Status:  metav1.StatusFailure,
		},
	}

}

// statusSucceed is a helper function to create a metav1 success status
func statusSucceed() metav1.Status {
	return metav1.Status{Status: metav1.StatusSuccess}
}

// isHub is a function to identify the runtime object is a hub or not
func isHub(obj runtime.Object) bool {
	_, yes := obj.(conversion.Hub)
	return yes
}

// isConvertible is a function to identify the runtime object is convertible or not
func isConvertible(obj runtime.Object) bool {
	_, yes := obj.(conversion.Convertible)
	return yes
}
