# Contact details and privacy rules

These rules are synthetic text written for the AlixAI reference application.

## Who sees which records

- A patient sees only their own record and their own appointments.
- A doctor sees only the patients who have an appointment with them.
- A receptionist sees every patient and appointment of their own clinic site and nothing of any other site, even of the same clinic group.
- Nobody sees the records of another clinic group.

## Changing contact details

- The phone number and the email address on file are used for booking and cancellation messages. Either one or both can be changed.
- A patient can change their own contact details. A receptionist can change the contact details of any patient of the site. A doctor cannot change contact details.
- A phone number holds at least 5 digits and only digits, spaces and the characters + - ( ) and a dot. An email address has the form name@domain.tld.
- A change of contact details is small and can be undone by changing it back, so the clinic applies it as soon as it has shown the new value; no separate confirmation is asked.

## Data kept

- Appointments and their cancellations are kept as part of the patient's record. Messages to patients are queued by the clinic and sent to the contact details on file.
- Searching for a patient shows the name and the date of birth, and only for the patients the person asking is allowed to see.
