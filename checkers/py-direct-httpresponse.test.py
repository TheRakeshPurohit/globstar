import urllib
import json
from django.db.models import Q
from django.auth import User
from django.http import HttpResponse, HttpResponseBadRequest
from django.utils.translation import ugettext as _

from org import engines, manageNoEngine, genericApiException

def search_certificates(request):
    user_filter = request.GET.get("user", "")
    if not user_filter:
        # <no-error>
        return HttpResponseBadRequest("user was not given")


    user = User.objects.get(Q(email=user_filter) | Q(username=user_filter))
    if user.DoesNotExist:
        # <expect-error>
        return HttpResponseBadRequest(_("user '{user}' does not exist").format(user_filter))

def previewNode(request, uid):
    """Preview evaluante node"""
    try:
        if uid in engines:
            _nodeId = request.data.get('nodeId')
            engines[uid].stoppable = True
            _res = engines[uid].model.previewNode(_nodeId)
            if _res is None:
                # <no-error>
                return HttpResponse('', status=204)
            # <expect-error>
            return HttpResponse(_res)
        return manageNoEngine()
    except Exception as e:
        return genericApiException(e, engines[uid])
    finally:
        engines[uid].stoppable = False

def inline_test(request):
    # <expect-error>
    return HttpResponse("Received {}".format(request.POST.get('message')))


def vote(request, question_id):
    if request.method != "GET" and request.method != "POST":
        # <expect-error>
        return HttpResponseBadRequest(
            "This view can not handle method {0}\n".format(request.method), status=405
        )

def endpoint():
    # <no-error>
    return HttpResponse(json.dumps({ 'status': 'ERROR', 'error': str(e) }), content_type='text/json')


def dangerous_endpoint():
    # <expect-error>
    return HttpResponse(json.dumps({ 'status': 'ERROR', 'error': str(e) }), content_type='text/html')
